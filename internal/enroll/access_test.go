package enroll

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func ptr[T any](v T) *T { return &v }

// The access rule is the product's paywall. It is a pure function of facts
// already loaded, so every branch is enumerable — and enumerated.
func TestDecide(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	learner := uuid.New()
	signedIn := Reader{UserID: learner}
	anonymous := Reader{}
	authorReader := Reader{UserID: uuid.New(), CanAuthor: true}

	published := func(preview bool) LessonView {
		return LessonView{
			Lesson:       LessonContent{IsPreview: preview},
			CourseStatus: "published",
		}
	}
	draft := func(preview bool) LessonView {
		v := published(preview)
		v.CourseStatus = "draft"
		return v
	}
	withEnrolment := func(v LessonView, status string, expires *time.Time) LessonView {
		v.EnrolmentStatus = &status
		v.EnrolmentExpires = expires
		return v
	}

	tests := []struct {
		name   string
		view   LessonView
		reader Reader
		want   Access
	}{
		// Authors see everything, including their own unpublished work.
		{"author reads a draft", draft(false), authorReader, AccessAuthor},
		{"author reads a published lesson", published(false), authorReader, AccessAuthor},

		// Nobody else may read anything in a draft — not even a lesson flagged as a
		// preview, because the course itself is not released.
		{"stranger cannot read a draft", draft(false), anonymous, AccessDenied},
		{"stranger cannot read a draft preview", draft(true), anonymous, AccessDenied},
		{"enrolled learner cannot read a draft", withEnrolment(draft(false), StatusActive, nil), signedIn, AccessDenied},

		// A preview of a published course is a free sample, for anyone who is not
		// already enrolled.
		{"stranger reads a preview", published(true), anonymous, AccessPreview},
		{"signed-in non-learner reads a preview", published(true), signedIn, AccessPreview},

		// But an enrolled learner reading a preview is enrolled, not previewing.
		// The order of the clauses in decide() is what makes this true, and it is
		// load-bearing: completing a lesson requires AccessEnrolled, so reversed,
		// no course with a preview lesson could ever reach 100%.
		{"enrolled learner reads a preview", withEnrolment(published(true), StatusActive, nil), signedIn, AccessEnrolled},
		{"enrolled learner reads a paid lesson", withEnrolment(published(false), StatusActive, nil), signedIn, AccessEnrolled},

		// Everything else needs a live enrolment.
		{"stranger denied a paid lesson", published(false), anonymous, AccessDenied},
		{"signed in but not enrolled", published(false), signedIn, AccessDenied},
		{"active enrolment", withEnrolment(published(false), StatusActive, nil), signedIn, AccessEnrolled},
		{"active enrolment, not yet expired", withEnrolment(published(false), StatusActive, &future), signedIn, AccessEnrolled},

		// Finishing a course does not evict you from it.
		{"completed enrolment still reads", withEnrolment(published(false), StatusCompleted, nil), signedIn, AccessEnrolled},

		// Expiry and cancellation do.
		{"expired by date", withEnrolment(published(false), StatusActive, &past), signedIn, AccessDenied},
		{"expired by status", withEnrolment(published(false), StatusExpired, nil), signedIn, AccessDenied},
		{"cancelled", withEnrolment(published(false), StatusCancelled, nil), signedIn, AccessDenied},

		// A cancelled learner keeps the preview everyone has.
		{"cancelled learner keeps preview", withEnrolment(published(true), StatusCancelled, nil), signedIn, AccessPreview},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decide(tt.view, tt.reader, now); got != tt.want {
				t.Errorf("decide = %v, want %v", got, tt.want)
			}
		})
	}
}

// The zero value must deny. A bug that forgets to assign an access level should
// lock the door, not open it.
func TestAccessZeroValueDenies(t *testing.T) {
	t.Parallel()

	var a Access
	if a.Granted() {
		t.Error("the zero Access value grants entry")
	}
	if a != AccessDenied {
		t.Error("the zero Access value is not AccessDenied")
	}
	if a.String() != "denied" {
		t.Errorf("String() = %q, want denied", a.String())
	}
}

func TestEnrolmentLive(t *testing.T) {
	t.Parallel()

	now := time.Now()

	tests := map[string]struct {
		enrolment Enrolment
		want      bool
	}{
		"active, no expiry":      {Enrolment{Status: StatusActive}, true},
		"active, future expiry":  {Enrolment{Status: StatusActive, ExpiresAt: ptr(now.Add(time.Hour))}, true},
		"active, past expiry":    {Enrolment{Status: StatusActive, ExpiresAt: ptr(now.Add(-time.Hour))}, false},
		"completed keeps access": {Enrolment{Status: StatusCompleted}, true},
		"cancelled":              {Enrolment{Status: StatusCancelled}, false},
		"expired":                {Enrolment{Status: StatusExpired}, false},
		"unknown status":         {Enrolment{Status: "nonsense"}, false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := tt.enrolment.Live(now); got != tt.want {
				t.Errorf("Live = %v, want %v", got, tt.want)
			}
		})
	}
}

// A course with no lessons is not complete, however tempting 0 == 0 looks.
func TestProgressComplete(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		progress Progress
		want     bool
	}{
		"empty course":  {Progress{LessonsCompleted: 0, LessonsTotal: 0}, false},
		"none done":     {Progress{LessonsCompleted: 0, LessonsTotal: 5}, false},
		"partway":       {Progress{LessonsCompleted: 3, LessonsTotal: 5}, false},
		"all done":      {Progress{LessonsCompleted: 5, LessonsTotal: 5}, true},
		"single lesson": {Progress{LessonsCompleted: 1, LessonsTotal: 1}, true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := tt.progress.Complete(); got != tt.want {
				t.Errorf("Complete = %v, want %v", got, tt.want)
			}
		})
	}
}
