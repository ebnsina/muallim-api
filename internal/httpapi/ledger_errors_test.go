package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/ebnsina/muallim-api/internal/ledger"
)

// The ledger sentinels go through their own mapper. A sentinel with no case here
// would fall through to a 500 "unexpected error".
func TestLedgerSentinelsMapToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := map[error]int{
		ledger.ErrNotFound:        http.StatusNotFound,
		ledger.ErrInvalidPage:     http.StatusUnprocessableEntity,
		ledger.ErrInvalidCategory: http.StatusUnprocessableEntity,
		ledger.ErrInvalidEntry:    http.StatusUnprocessableEntity,
	}

	for err, want := range tests {
		if got := statusOf(ledgerError(err)); got != want {
			t.Errorf("%v mapped to %d, want %d", err, got, want)
		}

		wrapped := fmt.Errorf("ledger: doing a thing: %w", err)
		if got := statusOf(ledgerError(wrapped)); got != want {
			t.Errorf("wrapped %v mapped to %d, want %d", err, got, want)
		}
	}
}

func TestAnUnmappedLedgerErrorIsFiveHundred(t *testing.T) {
	t.Parallel()

	if got := statusOf(ledgerError(errors.New("ledger: something new"))); got != http.StatusInternalServerError {
		t.Errorf("an unmapped ledger error mapped to %d, want 500", got)
	}
}
