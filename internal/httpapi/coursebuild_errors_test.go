package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/ebnsina/muallim-api/internal/coursebuild"
)

// The coursebuild sentinels go through their own mapper. A sentinel with no case
// here falls through to the default branch and becomes a 500 "unexpected error".
func TestCourseBuildSentinelsMapToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := map[error]int{
		coursebuild.ErrNotFound:         http.StatusNotFound,
		coursebuild.ErrInvalidBlueprint: http.StatusUnprocessableEntity,
		coursebuild.ErrInvalidPage:      http.StatusUnprocessableEntity,
	}

	for err, want := range tests {
		if got := statusOf(courseBuildError(err)); got != want {
			t.Errorf("%v mapped to %d, want %d", err, got, want)
		}

		// Wrapped sentinels must map identically: domains wrap with context.
		wrapped := fmt.Errorf("coursebuild: doing a thing: %w", err)
		if got := statusOf(courseBuildError(wrapped)); got != want {
			t.Errorf("wrapped %v mapped to %d, want %d", err, got, want)
		}
	}
}

func TestAnUnmappedCourseBuildErrorIsFiveHundred(t *testing.T) {
	t.Parallel()

	if got := statusOf(courseBuildError(errors.New("coursebuild: something new"))); got != http.StatusInternalServerError {
		t.Errorf("an unmapped coursebuild error mapped to %d, want 500", got)
	}
}
