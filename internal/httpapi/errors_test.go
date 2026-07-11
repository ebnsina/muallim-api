package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/lms-api/internal/assess"
	"github.com/ebnsina/lms-api/internal/assign"
	"github.com/ebnsina/lms-api/internal/auth"
	"github.com/ebnsina/lms-api/internal/catalog"
	"github.com/ebnsina/lms-api/internal/certify"
	"github.com/ebnsina/lms-api/internal/enroll"
	"github.com/ebnsina/lms-api/internal/grade"
	"github.com/ebnsina/lms-api/internal/learn"
	"github.com/ebnsina/lms-api/internal/platform/blob"
)

// statusOf extracts the HTTP status a mapped error will render as, or 500 when
// the error was never mapped and falls through as unexpected.
func statusOf(err error) int {
	var statusErr huma.StatusError
	if errors.As(err, &statusErr) {
		return statusErr.GetStatus()
	}
	return http.StatusInternalServerError
}

// Every domain sentinel must have a deliberate status. One that does not falls
// through the default branch and becomes a 500 — which is how "this workspace is
// invitation-only" once reached a user as "unexpected error occurred".
//
// A new sentinel added to a domain package without a line here will be caught by
// this table, provided the sentinel is listed. Listing it is the cheap part.
func TestEveryDomainSentinelMapsToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want int
	}{
		// auth
		{"invalid credentials", auth.ErrInvalidCredentials, http.StatusUnauthorized},
		{"unauthenticated", auth.ErrUnauthenticated, http.StatusUnauthorized},
		{"session invalid", auth.ErrSessionInvalid, http.StatusUnauthorized},
		{"session reused", auth.ErrSessionReused, http.StatusUnauthorized},
		{"forbidden", auth.ErrForbidden, http.StatusForbidden},
		{"registration closed", auth.ErrRegistrationClosed, http.StatusForbidden},
		{"email taken", auth.ErrEmailTaken, http.StatusConflict},
		{"password too short", auth.ErrPasswordTooShort, http.StatusUnprocessableEntity},
		{"password too long", auth.ErrPasswordTooLong, http.StatusUnprocessableEntity},
		{"credential token invalid", auth.ErrTokenInvalid, http.StatusBadRequest},

		// catalog
		{"course not found", catalog.ErrNotFound, http.StatusNotFound},
		{"invalid cursor", catalog.ErrInvalidPage, http.StatusUnprocessableEntity},
		{"invalid limit", catalog.ErrInvalidLimit, http.StatusUnprocessableEntity},
		{"invalid slug", catalog.ErrInvalidSlug, http.StatusUnprocessableEntity},
		{"slug taken", catalog.ErrSlugTaken, http.StatusConflict},
		{"invalid lesson", catalog.ErrInvalidLesson, http.StatusUnprocessableEntity},
		{"invalid video", catalog.ErrInvalidVideo, http.StatusUnprocessableEntity},
		{"incomplete order", catalog.ErrIncompleteOrder, http.StatusUnprocessableEntity},
		{"empty course", catalog.ErrEmptyCourse, http.StatusConflict},
		{"already published", catalog.ErrAlreadyPublished, http.StatusConflict},
		{"prerequisite cycle", catalog.ErrPrerequisiteCycle, http.StatusUnprocessableEntity},
		{"prerequisite exists", catalog.ErrPrerequisiteExists, http.StatusConflict},
		{"invalid drip mode", catalog.ErrInvalidDripMode, http.StatusUnprocessableEntity},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mapper := authError
			for _, catalogErr := range []error{
				catalog.ErrNotFound, catalog.ErrInvalidPage, catalog.ErrInvalidLimit,
				catalog.ErrInvalidSlug, catalog.ErrSlugTaken, catalog.ErrInvalidLesson,
				catalog.ErrInvalidVideo,
				catalog.ErrIncompleteOrder, catalog.ErrEmptyCourse, catalog.ErrAlreadyPublished,
				catalog.ErrPrerequisiteCycle, catalog.ErrPrerequisiteExists,
				catalog.ErrInvalidDripMode,
			} {
				if errors.Is(tt.err, catalogErr) {
					mapper = catalogError
					break
				}
			}

			if got := statusOf(mapper(tt.err)); got != tt.want {
				t.Errorf("%v mapped to %d, want %d", tt.err, got, tt.want)
			}

			// Wrapped sentinels must map identically: domains wrap with context.
			wrapped := fmt.Errorf("catalog: load thing: %w", tt.err)
			if got := statusOf(mapper(wrapped)); got != tt.want {
				t.Errorf("wrapped %v mapped to %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

// The membership sentinels go through their own mapper.
func TestMembershipSentinelsMapToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := map[error]int{
		auth.ErrInvitationInvalid:  http.StatusNotFound,
		auth.ErrInvalidCredentials: http.StatusUnauthorized,
		auth.ErrAlreadyMember:      http.StatusConflict,
		auth.ErrInvitationPending:  http.StatusConflict,
		auth.ErrRegistrationClosed: http.StatusForbidden,
		auth.ErrLastOwner:          http.StatusConflict,
		auth.ErrSelfModification:   http.StatusConflict,
		auth.ErrNotMember:          http.StatusNotFound,
		auth.ErrForbidden:          http.StatusForbidden,
	}

	for err, want := range tests {
		if got := statusOf(membersError(err)); got != want {
			t.Errorf("%v mapped to %d, want %d", err, got, want)
		}
	}
}

// The enrolment sentinels go through their own mapper.
func TestEnrolmentSentinelsMapToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := map[error]int{
		// A lesson a reader may not see is 404: they learn nothing about whether it
		// exists.
		enroll.ErrNotFound: http.StatusNotFound,

		// But "not enrolled" is 403: the course is published and visible, so there
		// is nothing to conceal, and the client should show an enrol button.
		enroll.ErrNotEnrolled: http.StatusForbidden,

		enroll.ErrAlreadyEnrolled: http.StatusConflict,
		enroll.ErrCourseNotOpen:   http.StatusConflict,
		enroll.ErrEnrolmentEnded:  http.StatusForbidden,

		// Also 403, and for the same reason: the course is visible, and the answer
		// is "finish those first" rather than "no such course".
		enroll.ErrPrerequisitesUnmet: http.StatusForbidden,

		// And a dripped lesson the learner can already see in the curriculum.
		enroll.ErrLessonLocked: http.StatusForbidden,
	}

	for err, want := range tests {
		if got := statusOf(enrolError(err)); got != want {
			t.Errorf("%v mapped to %d, want %d", err, got, want)
		}
		wrapped := fmt.Errorf("enroll: doing a thing: %w", err)
		if got := statusOf(enrolError(wrapped)); got != want {
			t.Errorf("wrapped %v mapped to %d, want %d", err, got, want)
		}
	}
}

// The typed error carries the courses. It must map to the same status as its
// sentinel, and its message must name them — a client should never have to parse
// prose to learn which course to finish.
func TestUnmetPrerequisitesNamesTheCourses(t *testing.T) {
	t.Parallel()

	err := &enroll.UnmetPrerequisites{Missing: []enroll.MissingCourse{
		{Slug: "go-basics", Title: "Go Basics"},
		{Slug: "pointers", Title: "Pointers"},
	}}

	if !errors.Is(err, enroll.ErrPrerequisitesUnmet) {
		t.Fatal("UnmetPrerequisites does not unwrap to its sentinel")
	}

	mapped := enrolError(err)
	if got := statusOf(mapped); got != http.StatusForbidden {
		t.Errorf("mapped to %d, want 403", got)
	}

	var statusErr huma.StatusError
	if !errors.As(mapped, &statusErr) {
		t.Fatal("no status error")
	}
	detail := statusErr.Error()
	for _, want := range []string{"Go Basics", "Pointers"} {
		if !strings.Contains(detail, want) {
			t.Errorf("message %q does not name %q", detail, want)
		}
	}

	// One missing course reads as a sentence, not as a list of one.
	single := enrolError(&enroll.UnmetPrerequisites{
		Missing: []enroll.MissingCourse{{Slug: "go-basics", Title: "Go Basics"}},
	})
	if !strings.Contains(single.Error(), "Finish Go Basics before") {
		t.Errorf("single-course message reads badly: %q", single.Error())
	}
}

// A locked lesson says when it opens, and admits when it cannot say.
func TestLockedLessonMessage(t *testing.T) {
	t.Parallel()

	opens := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	dated := enrolError(&enroll.LessonLocked{AvailableAt: &opens})
	if got := statusOf(dated); got != http.StatusForbidden {
		t.Errorf("dated lock mapped to %d, want 403", got)
	}
	if !strings.Contains(dated.Error(), "14 August 2026") {
		t.Errorf("message does not name the date: %q", dated.Error())
	}

	// Sequential drip opens on an event. Inventing a date would be worse than
	// admitting there is none.
	undated := enrolError(&enroll.LessonLocked{})
	if got := statusOf(undated); got != http.StatusForbidden {
		t.Errorf("undated lock mapped to %d, want 403", got)
	}
	if !strings.Contains(undated.Error(), "Finish the previous lesson") {
		t.Errorf("undated message reads badly: %q", undated.Error())
	}
}

// An error nobody anticipated must become a 500, not leak through as a 200 or a
// misleading 4xx.
func TestUnmappedErrorBecomes500(t *testing.T) {
	t.Parallel()

	mystery := errors.New("the database caught fire")

	if got := statusOf(authError(mystery)); got != http.StatusInternalServerError {
		t.Errorf("an unmapped error mapped to %d, want 500", got)
	}
	if got := statusOf(catalogError(mystery)); got != http.StatusInternalServerError {
		t.Errorf("an unmapped error mapped to %d, want 500", got)
	}
	if got := statusOf(enrolError(mystery)); got != http.StatusInternalServerError {
		t.Errorf("an unmapped error mapped to %d, want 500", got)
	}
}

// The assessment sentinels go through their own mapper.
//
// Two of them are deliberately indistinguishable to a client. An attempt that
// belongs to somebody else and an attempt that does not exist are both 404: the
// difference between them is exactly the fact a probe would be after.
func TestAssessmentSentinelsMapToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := map[error]int{
		assess.ErrNotFound:       http.StatusNotFound,
		assess.ErrNotYourAttempt: http.StatusNotFound,

		assess.ErrQuizExists:        http.StatusConflict,
		assess.ErrAttemptsExhausted: http.StatusConflict,
		assess.ErrAttemptClosed:     http.StatusConflict,
		assess.ErrAttemptInProgress: http.StatusConflict,
		assess.ErrAttemptExpired:    http.StatusConflict,
		assess.ErrEmptyQuiz:         http.StatusConflict,

		assess.ErrNotAwaitingReview: http.StatusConflict,

		// The machine graded it, and its verdict is not an instructor's to overturn.
		assess.ErrNotManual:    http.StatusUnprocessableEntity,
		assess.ErrInvalidGrade: http.StatusUnprocessableEntity,

		assess.ErrInvalidQuiz:     http.StatusUnprocessableEntity,
		assess.ErrInvalidQuestion: http.StatusUnprocessableEntity,
		assess.ErrIncompleteOrder: http.StatusUnprocessableEntity,
	}

	for err, want := range tests {
		if got := statusOf(assessError(err)); got != want {
			t.Errorf("%v mapped to %d, want %d", err, got, want)
		}

		// Wrapped sentinels must map identically: domains wrap with context.
		wrapped := fmt.Errorf("assess: doing a thing: %w", err)
		if got := statusOf(assessError(wrapped)); got != want {
			t.Errorf("wrapped %v mapped to %d, want %d", err, got, want)
		}
	}
}

// An unmapped error is a 500, and that is the point of the table above: a
// sentinel nobody gave a case to reaches a user as "unexpected error".
func TestAnUnmappedAssessmentErrorIsFiveHundred(t *testing.T) {
	t.Parallel()

	if got := statusOf(assessError(errors.New("assess: something new"))); got != http.StatusInternalServerError {
		t.Errorf("an unmapped error mapped to %d, want 500", got)
	}
}

// The assignment sentinels go through their own mapper.
//
// `ErrNotYours` and `ErrNotFound` are deliberately the same status. A key that
// belongs to somebody else and a key that does not exist look identical to
// whoever asked, because the difference between them is what a probe is after.
func TestAssignmentSentinelsMapToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := map[error]int{
		assign.ErrNotFound: http.StatusNotFound,
		assign.ErrNotYours: http.StatusNotFound,

		assign.ErrAssignmentExists: http.StatusConflict,
		assign.ErrAlreadySubmitted: http.StatusConflict,
		assign.ErrNothingToSubmit:  http.StatusConflict,
		assign.ErrNotSubmitted:     http.StatusConflict,
		assign.ErrTooManyFiles:     http.StatusConflict,
		assign.ErrPastDue:          http.StatusConflict,
		assign.ErrUploadMissing:    http.StatusConflict,

		assign.ErrInvalidAssignment: http.StatusUnprocessableEntity,
		assign.ErrInvalidFile:       http.StatusUnprocessableEntity,
		assign.ErrInvalidGrade:      http.StatusUnprocessableEntity,

		// Not a 500. Nothing is broken; this deployment has no bucket.
		blob.ErrNotConfigured: http.StatusServiceUnavailable,
	}

	for err, want := range tests {
		if got := statusOf(assignError(err)); got != want {
			t.Errorf("%v mapped to %d, want %d", err, got, want)
		}

		wrapped := fmt.Errorf("assign: doing a thing: %w", err)
		if got := statusOf(assignError(wrapped)); got != want {
			t.Errorf("wrapped %v mapped to %d, want %d", err, got, want)
		}
	}
}

func TestAnUnmappedAssignmentErrorIsFiveHundred(t *testing.T) {
	t.Parallel()

	if got := statusOf(assignError(errors.New("assign: something new"))); got != http.StatusInternalServerError {
		t.Errorf("an unmapped error mapped to %d, want 500", got)
	}
}

// The gradebook's sentinels go through their own mapper.
func TestGradeSentinelsMapToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := map[error]int{
		grade.ErrNotFound:     http.StatusNotFound,
		grade.ErrScaleExists:  http.StatusConflict,
		grade.ErrInvalidScale: http.StatusUnprocessableEntity,

		// Not a 4xx. A malformed score is one this system built, not one a client
		// sent, so it renders as the bug it is: a 500 with a correlation id.
		grade.ErrInvalidScore: http.StatusInternalServerError,
	}

	for err, want := range tests {
		if got := statusOf(gradeError(err)); got != want {
			t.Errorf("%v mapped to %d, want %d", err, got, want)
		}

		wrapped := fmt.Errorf("grade: doing a thing: %w", err)
		if got := statusOf(gradeError(wrapped)); got != want {
			t.Errorf("wrapped %v mapped to %d, want %d", err, got, want)
		}
	}
}

func TestAnUnmappedGradeErrorIsFiveHundred(t *testing.T) {
	t.Parallel()

	if got := statusOf(gradeError(errors.New("grade: something new"))); got != http.StatusInternalServerError {
		t.Errorf("an unmapped error mapped to %d, want 500", got)
	}
}

// The certify sentinels go through their own mapper.
func TestCertifySentinelsMapToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := map[error]int{
		certify.ErrNotFound:        http.StatusNotFound,
		certify.ErrTemplateExists:  http.StatusConflict,
		certify.ErrRevoked:         http.StatusConflict,
		certify.ErrInvalidTemplate: http.StatusUnprocessableEntity,
	}

	for err, want := range tests {
		if got := statusOf(certifyError(err)); got != want {
			t.Errorf("%v mapped to %d, want %d", err, got, want)
		}

		wrapped := fmt.Errorf("certify: doing a thing: %w", err)
		if got := statusOf(certifyError(wrapped)); got != want {
			t.Errorf("wrapped %v mapped to %d, want %d", err, got, want)
		}
	}
}

// The learn sentinels go through their own mapper.
func TestLearnSentinelsMapToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := map[error]int{
		learn.ErrLessonNotFound:    http.StatusNotFound,
		learn.ErrHighlightNotFound: http.StatusNotFound,
		learn.ErrInvalidHighlight:  http.StatusUnprocessableEntity,
		learn.ErrNoteTooLong:       http.StatusUnprocessableEntity,
	}

	for err, want := range tests {
		if got := statusOf(learnError(err)); got != want {
			t.Errorf("%v mapped to %d, want %d", err, got, want)
		}

		wrapped := fmt.Errorf("learn: doing a thing: %w", err)
		if got := statusOf(learnError(wrapped)); got != want {
			t.Errorf("wrapped %v mapped to %d, want %d", err, got, want)
		}
	}
}

func TestAnUnmappedLearnErrorIsFiveHundred(t *testing.T) {
	t.Parallel()

	if got := statusOf(learnError(errors.New("learn: something new"))); got != http.StatusInternalServerError {
		t.Errorf("an unmapped learn error mapped to %d, want 500", got)
	}
}

func TestAnUnmappedCertifyErrorIsFiveHundred(t *testing.T) {
	t.Parallel()

	if got := statusOf(certifyError(errors.New("certify: something new"))); got != http.StatusInternalServerError {
		t.Errorf("an unmapped error mapped to %d, want 500", got)
	}
}
