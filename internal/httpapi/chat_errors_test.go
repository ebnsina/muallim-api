package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/ebnsina/muallim-api/internal/chat"
)

func TestChatSentinelsMapToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := map[error]int{
		chat.ErrNotFound:    http.StatusNotFound,
		chat.ErrNotMember:   http.StatusForbidden,
		chat.ErrInvalid:     http.StatusUnprocessableEntity,
		chat.ErrInvalidPage: http.StatusUnprocessableEntity,
	}

	for err, want := range tests {
		if got := statusOf(chatError(err)); got != want {
			t.Errorf("%v mapped to %d, want %d", err, got, want)
		}
		wrapped := fmt.Errorf("chat: doing a thing: %w", err)
		if got := statusOf(chatError(wrapped)); got != want {
			t.Errorf("wrapped %v mapped to %d, want %d", err, got, want)
		}
	}
}

func TestAnUnmappedChatErrorIsFiveHundred(t *testing.T) {
	t.Parallel()

	if got := statusOf(chatError(errors.New("chat: something new"))); got != http.StatusInternalServerError {
		t.Errorf("an unmapped chat error mapped to %d, want 500", got)
	}
}
