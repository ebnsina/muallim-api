package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/ebnsina/muallim-api/internal/transport"
)

// Every transport sentinel must map to a deliberate, non-500 status — wrapped and
// unwrapped alike, since the domain wraps its sentinels with context.
func TestTransportSentinelsMapToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := map[error]int{
		transport.ErrNotFound:          http.StatusNotFound,
		transport.ErrAlreadyAssigned:   http.StatusConflict,
		transport.ErrInvalidPage:       http.StatusUnprocessableEntity,
		transport.ErrInvalidRoute:      http.StatusUnprocessableEntity,
		transport.ErrInvalidVehicle:    http.StatusUnprocessableEntity,
		transport.ErrInvalidAssignment: http.StatusUnprocessableEntity,
	}

	for err, want := range tests {
		if got := statusOf(transportError(err)); got != want {
			t.Errorf("%v mapped to %d, want %d", err, got, want)
		}
		wrapped := fmt.Errorf("transport: doing a thing: %w", err)
		if got := statusOf(transportError(wrapped)); got != want {
			t.Errorf("wrapped %v mapped to %d, want %d", err, got, want)
		}
	}
}

func TestAnUnmappedTransportErrorIsFiveHundred(t *testing.T) {
	t.Parallel()

	if got := statusOf(transportError(errors.New("transport: something new"))); got != http.StatusInternalServerError {
		t.Errorf("an unmapped transport error mapped to %d, want 500", got)
	}
}
