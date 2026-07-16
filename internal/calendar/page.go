package calendar

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// cursor is the keyset position on the calendar, newest first: a page resumes at
// the row just before this start date and id.
type cursor struct {
	StartsOn time.Time `json:"s"`
	ID       uuid.UUID `json:"i"`
}

// PageParams is a request for one page of the calendar.
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

func encodeCursor(e Event) string {
	raw, _ := json.Marshal(cursor{StartsOn: e.StartsOn, ID: e.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// Page is one page of the calendar with a keyset cursor to the next.
type Page struct {
	Events     []Event
	NextCursor string
	HasMore    bool
}
