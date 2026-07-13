package auth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidPage means a cursor this API did not issue, or one it can no longer read.
var ErrInvalidPage = errors.New("auth: the page cursor is not valid")

// MaxPageSize bounds a page of members or invitations. A workspace can hold a
// school; a response cannot.
const MaxPageSize = 100

/*
cursor is a keyset position: the sort key of the last row of the page before.

Keyset and not OFFSET. `OFFSET 10000` makes Postgres read and throw away ten
thousand rows, so the last page of a large school costs many times the first, and a
member who joins mid-scroll shifts every page after them. A keyset seeks straight to
the position on the same index that satisfies the sort: every page costs the same,
and nobody is listed twice or skipped.

Before this, a workspace with more than two hundred members simply could not be
read to the end. The list stopped, and said nothing about stopping.
*/
type cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// encode renders it opaque, so a client cannot come to depend on a sort key we must
// stay free to change.
func (c cursor) encode() string {
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID.String()
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

	createdAt, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: bad timestamp", ErrInvalidPage)
	}

	parsedID, err := uuid.Parse(id)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: bad id", ErrInvalidPage)
	}

	return cursor{CreatedAt: createdAt, ID: parsedID}, nil
}

// Page is one page of rows, and where the next one starts.
type Page[T any] struct {
	Items      []T
	NextCursor string
	HasMore    bool
}

// PageParams is what a caller asks for: how many, and from where.
type PageParams struct {
	Limit  int
	Cursor string
}

// clamp bounds the size and decodes the position. A limit nobody set is a limit we
// set: no list endpoint here is unbounded.
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

/*
page turns limit+1 rows into a page and the position after it.

The extra row is how "is there more" is answered without a COUNT(*), which would
scan the whole table to say a thing the client only needs as a yes or a no.
*/
func page[T any](rows []T, limit int, key func(T) cursor) Page[T] {
	if len(rows) <= limit {
		return Page[T]{Items: rows}
	}

	rows = rows[:limit]
	return Page[T]{
		Items:      rows,
		HasMore:    true,
		NextCursor: key(rows[len(rows)-1]).encode(),
	}
}
