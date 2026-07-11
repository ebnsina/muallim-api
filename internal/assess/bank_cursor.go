package assess

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// bankCursor is a keyset position in the bank list: (created_at, id) descending.
type bankCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

func (c bankCursor) encode() string {
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeBankCursor(token string) (bankCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return bankCursor{}, fmt.Errorf("%w: not base64", ErrInvalidPage)
	}
	ts, id, found := strings.Cut(string(raw), "|")
	if !found {
		return bankCursor{}, fmt.Errorf("%w: malformed", ErrInvalidPage)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return bankCursor{}, fmt.Errorf("%w: bad timestamp", ErrInvalidPage)
	}
	parsedID, err := uuid.Parse(id)
	if err != nil {
		return bankCursor{}, fmt.Errorf("%w: bad id", ErrInvalidPage)
	}
	return bankCursor{CreatedAt: createdAt, ID: parsedID}, nil
}
