package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/ebnsina/muallim-api/internal/admissions"
)

func TestAdmissionsSentinelsMapToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := map[error]int{
		admissions.ErrNotFound:           http.StatusNotFound,
		admissions.ErrNotPending:         http.StatusConflict,
		admissions.ErrInvalidApplication: http.StatusUnprocessableEntity,
		admissions.ErrInvalidPage:        http.StatusUnprocessableEntity,
	}

	for err, want := range tests {
		if got := statusOf(admissionsError(err)); got != want {
			t.Errorf("%v mapped to %d, want %d", err, got, want)
		}
		wrapped := fmt.Errorf("admissions: doing a thing: %w", err)
		if got := statusOf(admissionsError(wrapped)); got != want {
			t.Errorf("wrapped %v mapped to %d, want %d", err, got, want)
		}
	}
}

func TestAnUnmappedAdmissionsErrorIsFiveHundred(t *testing.T) {
	t.Parallel()

	if got := statusOf(admissionsError(errors.New("admissions: something new"))); got != http.StatusInternalServerError {
		t.Errorf("an unmapped admissions error mapped to %d, want 500", got)
	}
}
