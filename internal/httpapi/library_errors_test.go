package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/ebnsina/muallim-api/internal/library"
)

// The library sentinels go through their own mapper. A sentinel with no case here
// falls through to the default branch and becomes a 500 "unexpected error".
func TestLibrarySentinelsMapToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := map[error]int{
		library.ErrNotFound:        http.StatusNotFound,
		library.ErrNoCopies:        http.StatusConflict,
		library.ErrAlreadyReturned: http.StatusConflict,
		library.ErrInvalidBook:     http.StatusUnprocessableEntity,
		library.ErrInvalidLoan:     http.StatusUnprocessableEntity,
		library.ErrInvalidPage:     http.StatusUnprocessableEntity,
	}

	for err, want := range tests {
		if got := statusOf(libraryError(err)); got != want {
			t.Errorf("%v mapped to %d, want %d", err, got, want)
		}

		// Wrapped sentinels must map identically: domains wrap with context.
		wrapped := fmt.Errorf("library: doing a thing: %w", err)
		if got := statusOf(libraryError(wrapped)); got != want {
			t.Errorf("wrapped %v mapped to %d, want %d", err, got, want)
		}
	}
}

func TestAnUnmappedLibraryErrorIsFiveHundred(t *testing.T) {
	t.Parallel()

	if got := statusOf(libraryError(errors.New("library: something new"))); got != http.StatusInternalServerError {
		t.Errorf("an unmapped library error mapped to %d, want 500", got)
	}
}
