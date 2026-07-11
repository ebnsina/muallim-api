package forum

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// threadCursor is the keyset position of a thread list: (pinned, last_activity,
// id), the same tuple the list is ordered by, all descending. Pinned first, then
// most recently active.
type threadCursor struct {
	Pinned   bool
	Activity time.Time
	ID       uuid.UUID
}

func (c threadCursor) encode() string {
	raw := strconv.FormatBool(c.Pinned) + "|" +
		c.Activity.UTC().Format(time.RFC3339Nano) + "|" + c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeThreadCursor(token string) (threadCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return threadCursor{}, fmt.Errorf("%w: not base64", ErrInvalidPage)
	}
	parts := strings.SplitN(string(raw), "|", 3)
	if len(parts) != 3 {
		return threadCursor{}, fmt.Errorf("%w: malformed", ErrInvalidPage)
	}
	pinned, err := strconv.ParseBool(parts[0])
	if err != nil {
		return threadCursor{}, fmt.Errorf("%w: bad pinned flag", ErrInvalidPage)
	}
	activity, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return threadCursor{}, fmt.Errorf("%w: bad timestamp", ErrInvalidPage)
	}
	id, err := uuid.Parse(parts[2])
	if err != nil {
		return threadCursor{}, fmt.Errorf("%w: bad id", ErrInvalidPage)
	}
	return threadCursor{Pinned: pinned, Activity: activity, ID: id}, nil
}

// postCursor is (created_at, id): a thread's replies read oldest first.
type postCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

func (c postCursor) encode() string {
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodePostCursor(token string) (postCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return postCursor{}, fmt.Errorf("%w: not base64", ErrInvalidPage)
	}
	ts, id, found := strings.Cut(string(raw), "|")
	if !found {
		return postCursor{}, fmt.Errorf("%w: malformed", ErrInvalidPage)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return postCursor{}, fmt.Errorf("%w: bad timestamp", ErrInvalidPage)
	}
	parsedID, err := uuid.Parse(id)
	if err != nil {
		return postCursor{}, fmt.Errorf("%w: bad id", ErrInvalidPage)
	}
	return postCursor{CreatedAt: createdAt, ID: parsedID}, nil
}
