package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/ebnsina/muallim-api/internal/taxonomy"
)

// The taxonomy sentinels go through their own mapper. A sentinel with no case here
// falls through to the default branch and becomes a 500 "unexpected error".
func TestTaxonomySentinelsMapToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := map[error]int{
		taxonomy.ErrNotFound:    http.StatusNotFound,
		taxonomy.ErrDuplicate:   http.StatusConflict,
		taxonomy.ErrInvalid:     http.StatusUnprocessableEntity,
		taxonomy.ErrInvalidPage: http.StatusUnprocessableEntity,
	}

	for err, want := range tests {
		if got := statusOf(taxonomyError(err)); got != want {
			t.Errorf("%v mapped to %d, want %d", err, got, want)
		}

		// Wrapped sentinels must map identically: domains wrap with context.
		wrapped := fmt.Errorf("taxonomy: doing a thing: %w", err)
		if got := statusOf(taxonomyError(wrapped)); got != want {
			t.Errorf("wrapped %v mapped to %d, want %d", err, got, want)
		}
	}
}

func TestAnUnmappedTaxonomyErrorIsFiveHundred(t *testing.T) {
	t.Parallel()

	if got := statusOf(taxonomyError(errors.New("taxonomy: something new"))); got != http.StatusInternalServerError {
		t.Errorf("an unmapped taxonomy error mapped to %d, want 500", got)
	}
}
