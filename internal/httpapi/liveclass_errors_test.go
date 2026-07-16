package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/ebnsina/muallim-api/internal/liveclass"
)

// The liveclass sentinels go through their own mapper. A sentinel with no case
// here would fall through to a 500 "unexpected error".
func TestLiveClassSentinelsMapToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := map[error]int{
		liveclass.ErrNotFound:       http.StatusNotFound,
		liveclass.ErrInvalidSession: http.StatusUnprocessableEntity,
		liveclass.ErrInvalidPage:    http.StatusUnprocessableEntity,
	}

	for err, want := range tests {
		if got := statusOf(liveClassError(err)); got != want {
			t.Errorf("%v mapped to %d, want %d", err, got, want)
		}

		wrapped := fmt.Errorf("liveclass: doing a thing: %w", err)
		if got := statusOf(liveClassError(wrapped)); got != want {
			t.Errorf("wrapped %v mapped to %d, want %d", err, got, want)
		}
	}
}

func TestAnUnmappedLiveClassErrorIsFiveHundred(t *testing.T) {
	t.Parallel()

	if got := statusOf(liveClassError(errors.New("liveclass: something new"))); got != http.StatusInternalServerError {
		t.Errorf("an unmapped liveclass error mapped to %d, want 500", got)
	}
}
