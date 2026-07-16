package learnpath

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// cursor is the keyset position in the paths listing, ordered newest first.
type cursor struct {
	CreatedAt time.Time `json:"c"`
	ID        uuid.UUID `json:"i"`
}

// PageParams is a request for one page.
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

func encodeCursor(p Path) string {
	raw, _ := json.Marshal(cursor{CreatedAt: p.CreatedAt, ID: p.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// Page is one page of paths with a keyset cursor to the next.
type Page struct {
	Paths      []Path
	NextCursor string
	HasMore    bool
}
