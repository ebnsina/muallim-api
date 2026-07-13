package certify

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidPage means a cursor this API did not issue.
var ErrInvalidPage = errors.New("certify: the page cursor is not valid")

// MaxPageSize bounds a page of certificates. A school issues them every term; a
// response holds one page of them.
const MaxPageSize = 100

/*
cursor is a keyset position: the sort key of the last row of the page before.

Keyset and not OFFSET, for the usual reason — `OFFSET 20000` reads and discards
twenty thousand rows to serve the last page — and because the list is newest-first:
a certificate issued mid-scroll would otherwise push every later page along by one
and hide a row behind the reader's back.
*/
type cursor struct {
	IssuedAt time.Time
	ID       uuid.UUID
}

func (c cursor) encode() string {
	raw := c.IssuedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(token string) (cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: not base64", ErrInvalidPage)
	}

	ts, id, found := strings.Cut(string(raw), "|")
	if !found {
		return cursor{}, fmt.Errorf("%w: malformed", ErrInvalidPage)
	}

	issuedAt, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: bad timestamp", ErrInvalidPage)
	}

	parsedID, err := uuid.Parse(id)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: bad id", ErrInvalidPage)
	}

	return cursor{IssuedAt: issuedAt, ID: parsedID}, nil
}

// Page is one page of certificates, and where the next begins.
type Page struct {
	Certificates []Certificate
	NextCursor   string
	HasMore      bool
}

// PageParams is how many, and from where.
type PageParams struct {
	Limit  int
	Cursor string
}

func (p PageParams) clamp() (int, *cursor, error) {
	limit := p.Limit
	if limit <= 0 || limit > MaxPageSize {
		limit = MaxPageSize
	}

	if p.Cursor == "" {
		return limit, nil, nil
	}

	after, err := decodeCursor(p.Cursor)
	if err != nil {
		return 0, nil, err
	}
	return limit, &after, nil
}

// paginate turns limit+1 rows into a page. The extra row answers "is there more"
// without a COUNT(*), which would read the table to say yes or no.
func paginate(rows []Certificate, limit int) Page {
	if len(rows) <= limit {
		return Page{Certificates: rows}
	}

	rows = rows[:limit]
	last := rows[len(rows)-1]
	return Page{
		Certificates: rows,
		HasMore:      true,
		NextCursor:   cursor{IssuedAt: last.IssuedAt, ID: last.ID}.encode(),
	}
}
