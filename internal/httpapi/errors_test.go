package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/lms-api/internal/auth"
	"github.com/ebnsina/lms-api/internal/catalog"
	"github.com/ebnsina/lms-api/internal/enroll"
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

		// catalog
		{"course not found", catalog.ErrNotFound, http.StatusNotFound},
		{"invalid cursor", catalog.ErrInvalidPage, http.StatusUnprocessableEntity},
		{"invalid limit", catalog.ErrInvalidLimit, http.StatusUnprocessableEntity},
		{"invalid slug", catalog.ErrInvalidSlug, http.StatusUnprocessableEntity},
		{"slug taken", catalog.ErrSlugTaken, http.StatusConflict},
		{"invalid lesson", catalog.ErrInvalidLesson, http.StatusUnprocessableEntity},
		{"incomplete order", catalog.ErrIncompleteOrder, http.StatusUnprocessableEntity},
		{"empty course", catalog.ErrEmptyCourse, http.StatusConflict},
		{"already published", catalog.ErrAlreadyPublished, http.StatusConflict},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mapper := authError
			for _, catalogErr := range []error{
				catalog.ErrNotFound, catalog.ErrInvalidPage, catalog.ErrInvalidLimit,
				catalog.ErrInvalidSlug, catalog.ErrSlugTaken, catalog.ErrInvalidLesson,
				catalog.ErrIncompleteOrder, catalog.ErrEmptyCourse, catalog.ErrAlreadyPublished,
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
