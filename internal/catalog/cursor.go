package catalog

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// cursor is a keyset position: the sort key of the last row of the previous page.
//
// Keyset pagination, not OFFSET. `OFFSET 10000` makes Postgres read and discard
// ten thousand rows, so the last page of a large catalog costs far more than the
// first, and a row inserted mid-scroll shifts every subsequent page. A keyset
// seeks straight to the position using the same index that satisfies the sort,
// so every page costs the same and no row is skipped or repeated.
type cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// encode renders a cursor as an opaque token. It is base64 so that clients treat
// it as opaque rather than parsing it and depending on our sort key — which we
// must stay free to change.
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
