// Package chat owns messaging: conversations, their members, and the messages in
// them. A conversation is one of three kinds — a course channel (one per course),
// a 1:1 direct message, or an ad-hoc group.
//
// It knows nothing about HTTP. It returns its own sentinel errors, which
// internal/httpapi maps to status codes, and it does not import a sibling: the
// enrolment check a course channel needs is passed in by the caller, wired in cmd.
//
// This is the persistent + REST foundation. The realtime (WebSocket) fan-out is a
// separate layer that calls Send, IsMember, EnsureMember, Conversation and
// ListForUser on the Service.
package chat

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors. internal/httpapi maps them; nothing else does.
var (
	// ErrNotFound is a conversation that is not there, or one the caller may not
	// see. Both read the same so neither reveals the other.
	ErrNotFound = errors.New("chat: conversation not found")

	// ErrNotMember is a read or a send against a conversation the caller does not
	// belong to. It is a 403 — the resource may exist, but it is not theirs.
	ErrNotMember = errors.New("chat: not a member")

	// ErrInvalid is a malformed request: an empty body, a group with no members, a
	// direct conversation with oneself.
	ErrInvalid = errors.New("chat: invalid request")

	// ErrInvalidPage is an opaque cursor that did not decode.
	ErrInvalidPage = errors.New("chat: invalid page cursor")
)

// Conversation kinds.
const (
	KindCourse = "course"
	KindDirect = "direct"
	KindGroup  = "group"
)

// Member roles.
const (
	RoleMember = "member"
	RoleAdmin  = "admin"
)

// Bounds.
const (
	MaxBodyLength  = 20_000
	MaxTitleLength = 200
	MaxGroupSize   = 500

	DefaultPageSize = 30
	MaxPageSize     = 100
)

// Conversation is a thread of messages between members.
type Conversation struct {
	ID        uuid.UUID
	Kind      string
	CourseID  *uuid.UUID
	Title     string
	CreatedBy uuid.UUID

	LastMessageAt time.Time
	CreatedAt     time.Time

	// Members is filled only when a single conversation is loaded, never in a list.
	Members []Member

	// LastMessage and Unread are filled by ListForUser, so a client can render the
	// conversation list without a read per row.
	LastMessage *Message
	Unread      int
}

// Member is one participant in a conversation.
type Member struct {
	UserID     uuid.UUID
	Name       string
	Role       string
	LastReadAt *time.Time
}

// Message is one line in a conversation. SenderID is uuid.Nil when the sender's
// account was removed.
type Message struct {
	ID             uuid.UUID
	ConversationID uuid.UUID
	SenderID       uuid.UUID
	SenderName     string
	Body           string
	CreatedAt      time.Time
}

// MessagePage is a keyset page of messages, newest first.
type MessagePage struct {
	Messages   []Message
	NextCursor string
}

// ConversationPage is a keyset page of a user's conversations, most recently
// active first.
type ConversationPage struct {
	Conversations []Conversation
	NextCursor    string
}
