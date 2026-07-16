package liveclass

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// cursor is the keyset position in a course's session listing, ordered by start
// time descending then id descending — the same order the covering index carries.
type cursor struct {
	StartsAt time.Time `json:"s"`
	ID       uuid.UUID `json:"i"`
}

// PageParams is a request for one page of sessions.
type PageParams struct {
	Limit  int
	Cursor string
}

const (
	defaultPageLimit = 50
	maxPageLimit     = 100
)

func (p PageParams) clamp() int {
	switch {
	case p.Limit <= 0:
		return defaultPageLimit
	case p.Limit > maxPageLimit:
		return maxPageLimit
	default:
		return p.Limit
	}
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

func encodeCursor(s Session) string {
	raw, _ := json.Marshal(cursor{StartsAt: s.StartsAt, ID: s.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// Page is one page of sessions with a keyset cursor to the next.
type Page struct {
	Sessions   []Session
	NextCursor string
	HasMore    bool
}
