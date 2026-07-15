package notices

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared by its consumer.
type Repository interface {
	Create(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewNotice, channel string, postedBy uuid.UUID) (Notice, error)
	RecipientsFor(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, audience string, target *uuid.UUID) ([]Recipient, error)
	SetRecipientCount(ctx context.Context, tx pgx.Tx, tenantID, noticeID uuid.UUID, count int) error
	Notices(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *cursor, limit int) ([]Notice, error)
}

// Broadcaster delivers one notice to one recipient, in the posting transaction.
// Declared here; cmd/ wires the email enqueuer behind it. A message and its
// delivery job commit together — the reason the queue is in Postgres.
type Broadcaster interface {
	SendNotice(ctx context.Context, tx pgx.Tx, to, name, subject, body string) error
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

// Author identifies who did something.
type Author struct {
	UserID uuid.UUID
}

// Service holds the notice rules and owns transaction boundaries.
type Service struct {
	db          *database.DB
	repo        Repository
	broadcaster Broadcaster
	audit       AuditRecorder
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, broadcaster Broadcaster, recorder AuditRecorder) *Service {
	return &Service{db: db, repo: repo, broadcaster: broadcaster, audit: recorder}
}

// Post records a notice and enqueues its delivery to every guardian in the
// audience, all in one transaction: the notice, the fan-out jobs, and the audit
// line commit together or not at all. An audience with nobody to reach is refused
// before anything is written, so a notice that exists is a notice that went out.
func (s *Service) Post(ctx context.Context, tenantID uuid.UUID, n NewNotice, author Author) (Notice, error) {
	if err := n.validate(); err != nil {
		return Notice{}, err
	}
	var notice Notice
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		recipients, err := s.repo.RecipientsFor(ctx, tx, tenantID, n.Audience, n.TargetID)
		if err != nil {
			return err
		}
		if len(recipients) == 0 {
			return ErrNoRecipients
		}

		notice, err = s.repo.Create(ctx, tx, tenantID, n, ChannelEmail, author.UserID)
		if err != nil {
			return err
		}

		for _, r := range recipients {
			if err := s.broadcaster.SendNotice(ctx, tx, r.Email, r.Name, n.Title, n.Body); err != nil {
				return fmt.Errorf("notices: enqueue to %s: %w", r.Email, err)
			}
		}
		notice.RecipientCount = len(recipients)
		if err := s.repo.SetRecipientCount(ctx, tx, tenantID, notice.ID, notice.RecipientCount); err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionPosted,
			TargetType: "notice", TargetID: notice.ID.String(),
			Metadata: map[string]any{"audience": n.Audience, "recipients": notice.RecipientCount},
		})
	})
	return notice, err
}

// Board lists the workspace's notices, newest first, keyset-paginated.
func (s *Service) Board(ctx context.Context, tenantID uuid.UUID, p PageParams) (Page, error) {
	after, err := p.decode()
	if err != nil {
		return Page{}, err
	}
	limit := p.clamp()

	var page Page
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.repo.Notices(ctx, tx, tenantID, after, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			page.HasMore = true
			page.NextCursor = encodeCursor(rows[limit-1])
			rows = rows[:limit]
		}
		page.Notices = rows
		return nil
	})
	return page, err
}
