package transport

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// cursor is the keyset position in a listing, ordered newest first.
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

func encodeRouteCursor(r Route) string {
	raw, _ := json.Marshal(cursor{CreatedAt: r.CreatedAt, ID: r.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func encodeAssignmentCursor(a Assignment) string {
	raw, _ := json.Marshal(cursor{CreatedAt: a.CreatedAt, ID: a.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// RoutePage is one page of routes with a keyset cursor to the next.
type RoutePage struct {
	Routes     []Route
	NextCursor string
	HasMore    bool
}

// AssignmentPage is one page of assignments with a keyset cursor to the next.
type AssignmentPage struct {
	Assignments []Assignment
	NextCursor  string
	HasMore     bool
}
