package taxonomy

import (
	"encoding/base64"
	"encoding/json"

	"github.com/google/uuid"
)

// cursor is the keyset position in a listing, ordered by name then id — the
// category and tag listings share the (name, id) shape.
type cursor struct {
	Name string    `json:"n"`
	ID   uuid.UUID `json:"i"`
}

// PageParams is a request for one page.
type PageParams struct {
	Limit  int
	Cursor string
}

const (
	defaultPageLimit = 50
	maxPageLimit     = 200
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

func encodeCategoryCursor(c Category) string {
	raw, _ := json.Marshal(cursor{Name: c.Name, ID: c.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func encodeTagCursor(t Tag) string {
	raw, _ := json.Marshal(cursor{Name: t.Name, ID: t.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// CategoryPage is one page of categories with a keyset cursor to the next.
type CategoryPage struct {
	Categories []Category
	NextCursor string
	HasMore    bool
}

// TagPage is one page of tags with a keyset cursor to the next.
type TagPage struct {
	Tags       []Tag
	NextCursor string
	HasMore    bool
}
