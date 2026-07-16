package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/ebnsina/muallim-api/internal/learnpath"
)

// The learnpath sentinels go through their own mapper. A sentinel with no case
// here falls through to the default branch and becomes a 500 "unexpected error".
func TestLearnPathSentinelsMapToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := map[error]int{
		learnpath.ErrNotFound:        http.StatusNotFound,
		learnpath.ErrDuplicate:       http.StatusConflict,
		learnpath.ErrIncompleteOrder: http.StatusConflict,
		learnpath.ErrInvalid:         http.StatusUnprocessableEntity,
		learnpath.ErrInvalidPage:     http.StatusUnprocessableEntity,
	}

	for err, want := range tests {
		if got := statusOf(learnPathError(err)); got != want {
			t.Errorf("%v mapped to %d, want %d", err, got, want)
		}

		// Wrapped sentinels must map identically: domains wrap with context.
		wrapped := fmt.Errorf("learnpath: doing a thing: %w", err)
		if got := statusOf(learnPathError(wrapped)); got != want {
			t.Errorf("wrapped %v mapped to %d, want %d", err, got, want)
		}
	}
}

func TestAnUnmappedLearnPathErrorIsFiveHundred(t *testing.T) {
	t.Parallel()

	if got := statusOf(learnPathError(errors.New("learnpath: something new"))); got != http.StatusInternalServerError {
		t.Errorf("an unmapped learnpath error mapped to %d, want 500", got)
	}
}
