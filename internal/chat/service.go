package chat

import (
	"bytes"
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is where messaging lives. Every method takes a transaction, never
// the pool: app.tenant_id is bound transaction-locally.
type Repository interface {
	// EnsureDirect gets or creates the one direct conversation for a canonical
	// pair, reporting whether it was created so the caller knows to add members.
	EnsureDirect(ctx context.Context, tx pgx.Tx, tenantID, low, high, creator uuid.UUID) (Conversation, bool, error)

	// EnsureCourseChannel gets or creates a course's single channel.
	EnsureCourseChannel(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, title string) (Conversation, bool, error)

	// CreateGroup writes an ad-hoc group conversation.
	CreateGroup(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, title string, creator uuid.UUID) (Conversation, error)

	// ConversationByID returns one conversation, or ErrNotFound.
	ConversationByID(ctx context.Context, tx pgx.Tx, tenantID, convID uuid.UUID) (Conversation, error)

	// Members lists a conversation's participants, with their display names.
	Members(ctx context.Context, tx pgx.Tx, tenantID, convID uuid.UUID) ([]Member, error)

	// IsMember reports whether a user belongs to a conversation.
	IsMember(ctx context.Context, tx pgx.Tx, tenantID, convID, userID uuid.UUID) (bool, error)

	// AddMember adds a participant if absent, reporting whether it was added.
	AddMember(ctx context.Context, tx pgx.Tx, tenantID, convID, userID uuid.UUID, role string) (bool, error)

	// RemoveMember drops a participant, reporting whether one was there.
	RemoveMember(ctx context.Context, tx pgx.Tx, tenantID, convID, userID uuid.UUID) (bool, error)

	// ListForUser returns one keyset page of the user's conversations, most recently
	// active first, each with its last message and the user's unread count.
	ListForUser(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, after *cursor, limit int) ([]Conversation, error)

	// Messages returns one keyset page of a conversation's messages, newest first.
	Messages(ctx context.Context, tx pgx.Tx, tenantID, convID uuid.UUID, after *cursor, limit int) ([]Message, error)

	// InsertMessage writes a message and bumps the conversation's last_message_at in
	// the same call, so the list ordering never disagrees with the messages it sorts.
	InsertMessage(ctx context.Context, tx pgx.Tx, tenantID, convID, senderID uuid.UUID, body string) (Message, error)

	// MarkRead stamps the member's last_read_at, reporting whether they were a member.
	MarkRead(ctx context.Context, tx pgx.Tx, tenantID, convID, userID uuid.UUID) (bool, error)

	// CourseIDBySlug resolves a course to open its channel; ErrNotFound for no match.
	CourseIDBySlug(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string) (uuid.UUID, error)
}

// AuditRecorder writes an audit line in the transaction of the thing it describes.
type AuditRecorder interface {
	Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e AuditEntry) error
}

// AuditEntry is one line of the audit log.
type AuditEntry struct {
	ActorID    *uuid.UUID
	Action     string
	TargetType string
	TargetID   string
	Metadata   map[string]any
}

// Audit actions.
const (
	ActionGroupCreated  = "chat.group_created"
	ActionMemberAdded   = "chat.member_added"
	ActionMemberRemoved = "chat.member_removed"
)

// Author identifies who did something, for the audit trail.
type Author struct {
	UserID uuid.UUID
}

// Service holds the messaging rules and owns transaction boundaries.
type Service struct {
	db    *database.DB
	repo  Repository
	audit AuditRecorder
}

// NewService returns a Service. A nil recorder is not allowed — pass a no-op.
func NewService(db *database.DB, repo Repository, recorder AuditRecorder) *Service {
	return &Service{db: db, repo: repo, audit: recorder}
}

// EnsureDirect finds or creates the 1:1 conversation for a pair. The pair is
// canonical-ordered so the same two people always resolve to one conversation,
// whichever way round they are named. Naming oneself is ErrInvalid.
func (s *Service) EnsureDirect(ctx context.Context, tenantID, userA, userB uuid.UUID) (Conversation, error) {
	if userA == uuid.Nil || userB == uuid.Nil || userA == userB {
		return Conversation{}, ErrInvalid
	}
	low, high := canonicalPair(userA, userB)

	var conv Conversation
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, created, err := s.repo.EnsureDirect(ctx, tx, tenantID, low, high, userA)
		if err != nil {
			return err
		}
		if created {
			if _, err := s.repo.AddMember(ctx, tx, tenantID, c.ID, low, RoleMember); err != nil {
				return err
			}
			if _, err := s.repo.AddMember(ctx, tx, tenantID, c.ID, high, RoleMember); err != nil {
				return err
			}
		}
		conv = c
		return nil
	})
	return conv, err
}

// CreateGroup opens an ad-hoc group. The creator joins as admin; the named members
// join as members. A group needs a title and at least one other member.
func (s *Service) CreateGroup(ctx context.Context, tenantID uuid.UUID, title string, memberIDs []uuid.UUID, creator uuid.UUID) (Conversation, error) {
	title = strings.TrimSpace(title)
	if title == "" || len(title) > MaxTitleLength || creator == uuid.Nil {
		return Conversation{}, ErrInvalid
	}
	members := dedupeExcluding(memberIDs, creator)
	if len(members) == 0 || len(members) > MaxGroupSize {
		return Conversation{}, ErrInvalid
	}

	var conv Conversation
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := s.repo.CreateGroup(ctx, tx, tenantID, title, creator)
		if err != nil {
			return err
		}
		if _, err := s.repo.AddMember(ctx, tx, tenantID, c.ID, creator, RoleAdmin); err != nil {
			return err
		}
		for _, m := range members {
			if _, err := s.repo.AddMember(ctx, tx, tenantID, c.ID, m, RoleMember); err != nil {
				return err
			}
		}
		conv = c
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &creator, Action: ActionGroupCreated,
			TargetType: "chat_conversation", TargetID: c.ID.String(),
			Metadata: map[string]any{"title": title, "members": len(members)},
		})
	})
	return conv, err
}

// EnsureCourseChannel gets or creates a course's single channel. It does not add
// anyone — a learner joins by EnsureMember once their enrolment is checked.
func (s *Service) EnsureCourseChannel(ctx context.Context, tenantID, courseID uuid.UUID, title string) (Conversation, error) {
	var conv Conversation
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, _, err := s.repo.EnsureCourseChannel(ctx, tx, tenantID, courseID, strings.TrimSpace(title))
		conv = c
		return err
	})
	return conv, err
}

// EnsureMember adds a user to a conversation if they are not already in it — how an
// enrolled learner joins a course channel. Idempotent.
func (s *Service) EnsureMember(ctx context.Context, tenantID, convID, userID uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.repo.AddMember(ctx, tx, tenantID, convID, userID, RoleMember)
		return err
	})
}

// ListForUser returns one keyset page of the caller's conversations, each with its
// last message and unread count, most recently active first.
func (s *Service) ListForUser(ctx context.Context, tenantID, userID uuid.UUID, p PageParams) (ConversationPage, error) {
	after, err := p.decode()
	if err != nil {
		return ConversationPage{}, err
	}
	limit := p.clamp()

	var page ConversationPage
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.repo.ListForUser(ctx, tx, tenantID, userID, after, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			last := rows[limit-1]
			page.NextCursor = encodeCursor(last.LastMessageAt, last.ID)
			rows = rows[:limit]
		}
		page.Conversations = rows
		return nil
	})
	return page, err
}

// Conversation returns one conversation and its members, if the caller may see it.
// Membership is the caller's to check first via IsMember; this loads the row.
func (s *Service) Conversation(ctx context.Context, tenantID, convID uuid.UUID) (Conversation, error) {
	var conv Conversation
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := s.repo.ConversationByID(ctx, tx, tenantID, convID)
		if err != nil {
			return err
		}
		members, err := s.repo.Members(ctx, tx, tenantID, convID)
		if err != nil {
			return err
		}
		c.Members = members
		conv = c
		return nil
	})
	return conv, err
}

// IsMember reports whether a user belongs to a conversation — the gate every read
// and send is guarded by, and the check the realtime layer runs before a subscribe.
func (s *Service) IsMember(ctx context.Context, tenantID, convID, userID uuid.UUID) (bool, error) {
	var member bool
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		member, err = s.repo.IsMember(ctx, tx, tenantID, convID, userID)
		return err
	})
	return member, err
}

// Messages returns one keyset page of a conversation's messages, newest first,
// membership-guarded.
func (s *Service) Messages(ctx context.Context, tenantID, convID, userID uuid.UUID, p PageParams) (MessagePage, error) {
	after, err := p.decode()
	if err != nil {
		return MessagePage{}, err
	}
	limit := p.clamp()

	var page MessagePage
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.requireMember(ctx, tx, tenantID, convID, userID); err != nil {
			return err
		}
		rows, err := s.repo.Messages(ctx, tx, tenantID, convID, after, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			last := rows[limit-1]
			page.NextCursor = encodeCursor(last.CreatedAt, last.ID)
			rows = rows[:limit]
		}
		page.Messages = rows
		return nil
	})
	return page, err
}

// Send posts a message. Only a member may send; a non-member is ErrNotMember and a
// missing conversation is ErrNotFound.
func (s *Service) Send(ctx context.Context, tenantID, convID, senderID uuid.UUID, body string) (Message, error) {
	body = strings.TrimSpace(body)
	if body == "" || len(body) > MaxBodyLength {
		return Message{}, ErrInvalid
	}

	var msg Message
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.requireMember(ctx, tx, tenantID, convID, senderID); err != nil {
			return err
		}
		var err error
		msg, err = s.repo.InsertMessage(ctx, tx, tenantID, convID, senderID, body)
		return err
	})
	return msg, err
}

// MarkRead stamps the caller's read high-water mark. A non-member is ErrNotMember.
func (s *Service) MarkRead(ctx context.Context, tenantID, convID, userID uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ok, err := s.repo.MarkRead(ctx, tx, tenantID, convID, userID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotMember
		}
		return nil
	})
}

// AddMember adds a member to a group. The caller's authority to do so (group admin)
// is checked by the transport layer.
func (s *Service) AddMember(ctx context.Context, tenantID, convID, userID uuid.UUID, actor Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		conv, err := s.repo.ConversationByID(ctx, tx, tenantID, convID)
		if err != nil {
			return err
		}
		if conv.Kind != KindGroup {
			return ErrInvalid
		}
		added, err := s.repo.AddMember(ctx, tx, tenantID, convID, userID, RoleMember)
		if err != nil {
			return err
		}
		if !added {
			return nil
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &actor.UserID, Action: ActionMemberAdded,
			TargetType: "chat_conversation", TargetID: convID.String(),
			Metadata: map[string]any{"user_id": userID.String()},
		})
	})
}

// RemoveMember drops a member from a group.
func (s *Service) RemoveMember(ctx context.Context, tenantID, convID, userID uuid.UUID, actor Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		conv, err := s.repo.ConversationByID(ctx, tx, tenantID, convID)
		if err != nil {
			return err
		}
		if conv.Kind != KindGroup {
			return ErrInvalid
		}
		removed, err := s.repo.RemoveMember(ctx, tx, tenantID, convID, userID)
		if err != nil {
			return err
		}
		if !removed {
			return ErrNotMember
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &actor.UserID, Action: ActionMemberRemoved,
			TargetType: "chat_conversation", TargetID: convID.String(),
			Metadata: map[string]any{"user_id": userID.String()},
		})
	})
}

// CourseIDBySlug resolves a course so its channel can be opened.
func (s *Service) CourseIDBySlug(ctx context.Context, tenantID uuid.UUID, slug string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = s.repo.CourseIDBySlug(ctx, tx, tenantID, slug)
		return err
	})
	return id, err
}

// requireMember returns ErrNotFound when the conversation is absent and
// ErrNotMember when it exists but the user is not in it.
func (s *Service) requireMember(ctx context.Context, tx pgx.Tx, tenantID, convID, userID uuid.UUID) error {
	if _, err := s.repo.ConversationByID(ctx, tx, tenantID, convID); err != nil {
		return err
	}
	member, err := s.repo.IsMember(ctx, tx, tenantID, convID, userID)
	if err != nil {
		return err
	}
	if !member {
		return ErrNotMember
	}
	return nil
}

// canonicalPair orders two ids so a pair always maps to one direct conversation.
func canonicalPair(a, b uuid.UUID) (low, high uuid.UUID) {
	if bytes.Compare(a[:], b[:]) <= 0 {
		return a, b
	}
	return b, a
}

// dedupeExcluding returns the unique non-nil ids in order, dropping the excluded one.
func dedupeExcluding(ids []uuid.UUID, exclude uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]bool, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if id == uuid.Nil || id == exclude || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
