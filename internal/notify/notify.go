// Package notify owns a person's in-app notifications.
//
// A notification is private, like a note: it is answered by (tenant, user) and
// the service scopes every read and write to the recipient. Other domains do not
// import this one — a producer declares a small Notifier interface it owns, and
// cmd wires it over this service, so the notification row commits in the
// transaction of the event it describes.
//
// It knows nothing about HTTP. It returns its own sentinel errors.
package notify

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors. internal/httpapi maps them; nothing else does.
var (
	// ErrNotFound is a notification that is not there, or not the caller's to touch.
	// Both read the same: a person's id cannot reach another's row.
	ErrNotFound = errors.New("notify: not found")

	// ErrInvalidPage is an opaque cursor that did not decode.
	ErrInvalidPage = errors.New("notify: invalid page cursor")
)

// Kinds. A kind names the event, so a client can group, icon, or route it. New
// producers add a kind here in the same change that emits it.
const (
	// KindAnswer is a reply to a question the recipient asked.
	KindAnswer = "answer"

	// KindAnnouncement is a notice posted to a course the recipient takes.
	KindAnnouncement = "announcement"

	// KindGrade is a mark on the recipient's submission.
	KindGrade = "grade"
)

// Page bounds.
const (
	DefaultPageSize = 20
	MaxPageSize     = 50
)

// Notification is one item in a person's bell.
type Notification struct {
	ID     uuid.UUID
	UserID uuid.UUID

	Kind  string
	Title string
	Body  string
	Link  string

	ReadAt    *time.Time
	CreatedAt time.Time
}

// Read reports whether the recipient has seen it.
func (n Notification) Read() bool { return n.ReadAt != nil }

// Page is one keyset page of notifications, newest first.
type Page struct {
	Notifications []Notification
	NextCursor    string
}

// Preferences is how a person wants to be notified. Absent means the defaults —
// the digest is on — so a row exists only once someone opts out.
type Preferences struct {
	EmailDigest bool
}

// DefaultPreferences is what a person gets before they have chosen anything.
func DefaultPreferences() Preferences { return Preferences{EmailDigest: true} }

// DigestGroup is one person's worth of a digest: who to mail, and the unread,
// not-yet-digested notifications to summarise for them.
type DigestGroup struct {
	UserID uuid.UUID
	Email  string
	Name   string

	Notifications []Notification
}
