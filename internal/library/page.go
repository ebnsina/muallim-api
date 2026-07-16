package library

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// cursor is the keyset position in a listing, ordered newest first — the books
// catalogue and the loans board share the (created_at, id) shape.
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

func encodeBookCursor(b Book) string {
	raw, _ := json.Marshal(cursor{CreatedAt: b.CreatedAt, ID: b.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func encodeLoanCursor(l Loan) string {
	raw, _ := json.Marshal(cursor{CreatedAt: l.CreatedAt, ID: l.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// BookPage is one page of the catalogue with a keyset cursor to the next.
type BookPage struct {
	Books      []Book
	NextCursor string
	HasMore    bool
}

// LoanPage is one page of loans with a keyset cursor to the next.
type LoanPage struct {
	Loans      []Loan
	NextCursor string
	HasMore    bool
}
