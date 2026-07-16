package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/ebnsina/muallim-api/internal/certdesign"
)

// The certdesign sentinels go through their own mapper. A sentinel with no case
// here falls through to the default branch and becomes a 500 "unexpected error".
func TestCertDesignSentinelsMapToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := map[error]int{
		certdesign.ErrNotFound:      http.StatusNotFound,
		certdesign.ErrInvalidDesign: http.StatusUnprocessableEntity,
		certdesign.ErrInvalidLayout: http.StatusUnprocessableEntity,
		certdesign.ErrInvalidPage:   http.StatusUnprocessableEntity,
	}

	for err, want := range tests {
		if got := statusOf(certDesignError(err)); got != want {
			t.Errorf("%v mapped to %d, want %d", err, got, want)
		}

		// Wrapped sentinels must map identically: domains wrap with context.
		wrapped := fmt.Errorf("certdesign: doing a thing: %w", err)
		if got := statusOf(certDesignError(wrapped)); got != want {
			t.Errorf("wrapped %v mapped to %d, want %d", err, got, want)
		}
	}
}

func TestAnUnmappedCertDesignErrorIsFiveHundred(t *testing.T) {
	t.Parallel()

	if got := statusOf(certDesignError(errors.New("certdesign: something new"))); got != http.StatusInternalServerError {
		t.Errorf("an unmapped certdesign error mapped to %d, want 500", got)
	}
}
