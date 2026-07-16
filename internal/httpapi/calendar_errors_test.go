package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/ebnsina/muallim-api/internal/calendar"
)

func TestCalendarSentinelsMapToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := map[error]int{
		calendar.ErrNotFound:     http.StatusNotFound,
		calendar.ErrInvalidEvent: http.StatusUnprocessableEntity,
		calendar.ErrInvalidPage:  http.StatusUnprocessableEntity,
	}

	for err, want := range tests {
		if got := statusOf(calendarError(err)); got != want {
			t.Errorf("%v mapped to %d, want %d", err, got, want)
		}
		wrapped := fmt.Errorf("calendar: doing a thing: %w", err)
		if got := statusOf(calendarError(wrapped)); got != want {
			t.Errorf("wrapped %v mapped to %d, want %d", err, got, want)
		}
	}
}

func TestAnUnmappedCalendarErrorIsFiveHundred(t *testing.T) {
	t.Parallel()

	if got := statusOf(calendarError(errors.New("calendar: something new"))); got != http.StatusInternalServerError {
		t.Errorf("an unmapped calendar error mapped to %d, want 500", got)
	}
}
