package chat

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// PageParams is a request for one page of messages or conversations.
type PageParams struct {
	Limit  int
	Cursor string
}

func (p PageParams) clamp() int {
	switch {
	case p.Limit <= 0:
		return DefaultPageSize
	case p.Limit > MaxPageSize:
		return MaxPageSize
	default:
		return p.Limit
	}
}

// cursor is a keyset position: (created_at, id), the tuple both message and
// conversation lists order by. Messages keyset on their own created_at,
// conversations on last_message_at.
type cursor struct {
	At time.Time `json:"a"`
	ID uuid.UUID `json:"i"`
}

func (p PageParams) decode() (*cursor, error) {
	if p.Cursor == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(p.Cursor)
	if err != nil {
		return nil, ErrInvalidPage
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, ErrInvalidPage
	}
	return &c, nil
}

func encodeCursor(at time.Time, id uuid.UUID) string {
	raw, _ := json.Marshal(cursor{At: at, ID: id})
	return base64.RawURLEncoding.EncodeToString(raw)
}
