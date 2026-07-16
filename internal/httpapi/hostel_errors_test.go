package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/ebnsina/muallim-api/internal/hostel"
)

func TestHostelSentinelsMapToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := map[error]int{
		hostel.ErrNotFound:          http.StatusNotFound,
		hostel.ErrRoomFull:          http.StatusConflict,
		hostel.ErrAlreadyAllocated:  http.StatusConflict,
		hostel.ErrInvalidPage:       http.StatusUnprocessableEntity,
		hostel.ErrInvalidBuilding:   http.StatusUnprocessableEntity,
		hostel.ErrInvalidRoom:       http.StatusUnprocessableEntity,
		hostel.ErrInvalidAllocation: http.StatusUnprocessableEntity,
	}

	for err, want := range tests {
		if got := statusOf(hostelError(err)); got != want {
			t.Errorf("%v mapped to %d, want %d", err, got, want)
		}
		wrapped := fmt.Errorf("hostel: doing a thing: %w", err)
		if got := statusOf(hostelError(wrapped)); got != want {
			t.Errorf("wrapped %v mapped to %d, want %d", err, got, want)
		}
	}
}

func TestAnUnmappedHostelErrorIsFiveHundred(t *testing.T) {
	t.Parallel()

	if got := statusOf(hostelError(errors.New("hostel: something new"))); got != http.StatusInternalServerError {
		t.Errorf("an unmapped hostel error mapped to %d, want 500", got)
	}
}
