package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/ebnsina/muallim-api/internal/bundle"
)

// The bundle sentinels go through their own mapper. A sentinel with no case here
// falls through to the default branch and becomes a 500 "unexpected error".
func TestBundleSentinelsMapToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := map[error]int{
		bundle.ErrNotFound:    http.StatusNotFound,
		bundle.ErrDuplicate:   http.StatusConflict,
		bundle.ErrInvalid:     http.StatusUnprocessableEntity,
		bundle.ErrInvalidPage: http.StatusUnprocessableEntity,
	}

	for err, want := range tests {
		if got := statusOf(bundleError(err)); got != want {
			t.Errorf("%v mapped to %d, want %d", err, got, want)
		}

		// Wrapped sentinels must map identically: domains wrap with context.
		wrapped := fmt.Errorf("bundle: doing a thing: %w", err)
		if got := statusOf(bundleError(wrapped)); got != want {
			t.Errorf("wrapped %v mapped to %d, want %d", err, got, want)
		}
	}
}

func TestAnUnmappedBundleErrorIsFiveHundred(t *testing.T) {
	t.Parallel()

	if got := statusOf(bundleError(errors.New("bundle: something new"))); got != http.StatusInternalServerError {
		t.Errorf("an unmapped bundle error mapped to %d, want 500", got)
	}
}
