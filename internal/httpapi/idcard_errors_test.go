package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/ebnsina/muallim-api/internal/idcard"
)

// The idcard sentinels go through their own mapper. A sentinel with no case here
// falls through to the default branch and becomes a 500 "unexpected error".
func TestIDCardSentinelsMapToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := map[error]int{
		idcard.ErrNotFound:        http.StatusNotFound,
		idcard.ErrInvalidTemplate: http.StatusUnprocessableEntity,
		idcard.ErrInvalidLayout:   http.StatusUnprocessableEntity,
		idcard.ErrInvalidPage:     http.StatusUnprocessableEntity,
	}

	for err, want := range tests {
		if got := statusOf(idCardError(err)); got != want {
			t.Errorf("%v mapped to %d, want %d", err, got, want)
		}

		// Wrapped sentinels must map identically: domains wrap with context.
		wrapped := fmt.Errorf("idcard: doing a thing: %w", err)
		if got := statusOf(idCardError(wrapped)); got != want {
			t.Errorf("wrapped %v mapped to %d, want %d", err, got, want)
		}
	}
}

func TestAnUnmappedIDCardErrorIsFiveHundred(t *testing.T) {
	t.Parallel()

	if got := statusOf(idCardError(errors.New("idcard: something new"))); got != http.StatusInternalServerError {
		t.Errorf("an unmapped idcard error mapped to %d, want 500", got)
	}
}
