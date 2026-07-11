package notify

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// cursor is a keyset position: the (created_at, id) of the last row of the
// previous page. Keyset, not OFFSET — every page costs the same and no row is
// skipped or repeated when one arrives mid-scroll.
type cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// encode renders a cursor as an opaque base64 token, so a client treats it as
// opaque rather than depending on our sort key.
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
