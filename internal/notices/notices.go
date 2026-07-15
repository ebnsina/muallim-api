// Package notices lets a school post a message that fans out to guardians. It knows
// nothing about HTTP, and reaches the academic spine (students, guardians) by id.
// Delivery is done by a Broadcaster the caller wires in — email today.
package notices

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Sentinels. internal/httpapi maps each to a status; add a line to errors_test.go
// in the same commit as a new one.
var (
	ErrNotFound       = errors.New("notices: not found")
	ErrInvalidNotice  = errors.New("notices: the notice is not valid")
	ErrNoRecipients   = errors.New("notices: the audience has nobody with a contact")
	ErrTargetRequired = errors.New("notices: this audience needs a class or section")
	ErrInvalidPage    = errors.New("notices: the page cursor is not valid")
)

// Audiences a notice can reach.
const (
	AudienceAll     = "all_guardians"
	AudienceClass   = "class_guardians"
	AudienceSection = "section_guardians"
)

// Channels a notice can go out on. Only email is wired today; sms follows.
const (
	ChannelEmail = "email"
)

// MaxPage bounds a page of the notice board.
const MaxPage = 100

// Audit action.
const ActionPosted = "notice.posted"

// Notice is a message posted to guardians.
type Notice struct {
	ID             uuid.UUID
	Title          string
	Body           string
	Audience       string
	TargetID       *uuid.UUID
	Channel        string
	RecipientCount int
	CreatedAt      time.Time
}

// NewNotice is a notice to post.
type NewNotice struct {
	Title    string
	Body     string
	Audience string
	TargetID *uuid.UUID
}

// Recipient is one person a notice reaches.
type Recipient struct {
	Name  string
	Email string
}

func (n NewNotice) validate() error {
	if n.Title == "" {
		return fmt.Errorf("%w: give it a title", ErrInvalidNotice)
	}
	if n.Body == "" {
		return fmt.Errorf("%w: give it a body", ErrInvalidNotice)
	}
	switch n.Audience {
	case AudienceAll:
	case AudienceClass, AudienceSection:
		if n.TargetID == nil {
			return ErrTargetRequired
		}
	default:
		return fmt.Errorf("%w: %q is not an audience", ErrInvalidNotice, n.Audience)
	}
	return nil
}

// cursor is the keyset position in the notice board, newest first.
type cursor struct {
	CreatedAt time.Time `json:"c"`
	ID        uuid.UUID `json:"i"`
}

// PageParams is a request for one page of the board.
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

func encodeCursor(n Notice) string {
	raw, _ := json.Marshal(cursor{CreatedAt: n.CreatedAt, ID: n.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// Page is one page of the board with a keyset cursor to the next.
type Page struct {
	Notices    []Notice
	NextCursor string
	HasMore    bool
}
