// Package forum owns community discussion: spaces, threads, and replies.
//
// It is broader than a lesson's Q&A. A space is a board — either workspace-wide,
// seen by any member, or bound to a course and seen by whoever may take it,
// decided in the query by a join to enrolments (the shared-schema read, no
// import of enroll). Threads live in a space; posts are replies.
//
// It knows nothing about HTTP. It returns its own sentinel errors, and it does
// not import a sibling: a producer interface (Notifier) is declared here and
// wired in cmd.
package forum

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors. internal/httpapi maps them; nothing else does.
var (
	// ErrSpaceNotFound is a space that is not there, or one the caller may not see.
	// Both read the same, so neither reveals the other.
	ErrSpaceNotFound = errors.New("forum: space not found")

	// ErrThreadNotFound is a thread that is not there or not visible to the caller.
	ErrThreadNotFound = errors.New("forum: thread not found")

	// ErrPostNotFound is a post another member asked to remove, or one that was
	// never there.
	ErrPostNotFound = errors.New("forum: post not found")

	// ErrLocked is a reply to a thread a moderator has closed.
	ErrLocked = errors.New("forum: thread is locked")

	// ErrEmpty is a thread or post with no body, or a thread with no title.
	ErrEmpty = errors.New("forum: title or body is empty")

	// ErrTooLong is a title or body past its bound.
	ErrTooLong = errors.New("forum: title or body is too long")

	// ErrInvalidPage is an opaque cursor that did not decode.
	ErrInvalidPage = errors.New("forum: invalid page cursor")

	// ErrForbidden is a thread or post the caller may neither own nor moderate — a
	// distinct answer from "not found", used when the resource is plainly visible
	// and the caller simply may not act on it.
	ErrForbidden = errors.New("forum: not allowed")
)

// Bounds.
const (
	MaxTitleLength = 200
	MaxBodyLength  = 20_000

	DefaultPageSize = 20
	MaxPageSize     = 50
)

// KindReply names the "new reply to your thread" notification.
const KindReply = "reply"

// Actor is who is acting on the forum. CanModerate is an authorisation decision
// from the transport layer (it maps to forum:moderate), never a request field.
type Actor struct {
	UserID      uuid.UUID
	CanModerate bool
}

// Space is a board.
type Space struct {
	ID       uuid.UUID
	CourseID *uuid.UUID // nil for a workspace-wide space
	Title    string
	Desc     string
	Position int

	// CourseSlug and CourseTitle are filled for a course-bound space, so a client
	// can label and link it without a second read.
	CourseSlug  string
	CourseTitle string

	ThreadCount int
	CreatedAt   time.Time
}

// Workspace reports whether the space is the whole community's, not a course's.
func (s Space) Workspace() bool { return s.CourseID == nil }

// Thread is a titled discussion with an opening body and a running reply tally.
type Thread struct {
	ID       uuid.UUID
	SpaceID  uuid.UUID
	AuthorID uuid.UUID

	Title string
	Body  string

	Pinned bool
	Locked bool

	ReplyCount int

	AuthorName     string
	LastActivityAt time.Time
	CreatedAt      time.Time
}

// Post is one reply in a thread.
type Post struct {
	ID       uuid.UUID
	ThreadID uuid.UUID
	AuthorID uuid.UUID

	Body       string
	AuthorName string
	CreatedAt  time.Time
}

// ThreadPage and PostPage are keyset pages, newest-active and oldest-first
// respectively.
type ThreadPage struct {
	Threads    []Thread
	NextCursor string
}

type PostPage struct {
	Posts      []Post
	NextCursor string
}

// Notification is a message this package asks to send — the author of a thread
// when someone replies. Restated here rather than imported; cmd wires an adapter
// over the notify service. Empty UserID means nobody to tell.
type Notification struct {
	UserID uuid.UUID
	Kind   string
	Title  string
	Body   string
	Link   string
}
