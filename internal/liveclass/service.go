package liveclass

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared by its consumer. Every method
// takes a transaction, never the pool: the tenant is bound transaction-locally.
type Repository interface {
	Create(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, n NewSession) (Session, error)
	ForCourse(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, after *cursor, limit int) ([]Session, error)
	ByID(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Session, error)
	Update(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, s Session) (Session, error)
	Delete(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error
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

// Author identifies who did something, for the audit trail.
type Author struct {
	UserID uuid.UUID
}

// Service holds the live-session rules and owns transaction boundaries.
type Service struct {
	db    *database.DB
	repo  Repository
	audit AuditRecorder
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, recorder AuditRecorder) *Service {
	return &Service{db: db, repo: repo, audit: recorder}
}

// Create schedules a live session on a course. The caller has already resolved the
// course and authorised the write; the host is the instructor doing so.
func (s *Service) Create(ctx context.Context, tenantID, courseID uuid.UUID, n NewSession, author Author) (Session, error) {
	if err := n.validate(); err != nil {
		return Session{}, err
	}
	var out Session
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.repo.Create(ctx, tx, tenantID, courseID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionCreated,
			TargetType: "live_session", TargetID: out.ID.String(),
			Metadata: map[string]any{"course_id": courseID.String(), "title": out.Title},
		})
	})
	return out, err
}

// ListForCourse returns a course's sessions, newest/soonest first, keyset-paginated.
func (s *Service) ListForCourse(ctx context.Context, tenantID, courseID uuid.UUID, p PageParams) (Page, error) {
	after, err := p.decode()
	if err != nil {
		return Page{}, err
	}
	limit := p.clamp()

	var page Page
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.repo.ForCourse(ctx, tx, tenantID, courseID, after, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			page.HasMore = true
			page.NextCursor = encodeCursor(rows[limit-1])
			rows = rows[:limit]
		}
		page.Sessions = rows
		return nil
	})
	return page, err
}

// Get loads one session by id.
func (s *Service) Get(ctx context.Context, tenantID, id uuid.UUID) (Session, error) {
	var out Session
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.repo.ByID(ctx, tx, tenantID, id)
		return err
	})
	return out, err
}

// Update applies a patch to a session. The patch is checked against the session it
// produces, so a backwards range or a bad link is refused however many requests it
// takes.
func (s *Service) Update(ctx context.Context, tenantID, id uuid.UUID, patch SessionPatch, author Author) (Session, error) {
	var out Session
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		current, err := s.repo.ByID(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if err := patch.apply(&current); err != nil {
			return err
		}
		out, err = s.repo.Update(ctx, tx, tenantID, current)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionUpdated,
			TargetType: "live_session", TargetID: out.ID.String(),
		})
	})
	return out, err
}

// Delete removes a session.
func (s *Service) Delete(ctx context.Context, tenantID, id uuid.UUID, author Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.repo.Delete(ctx, tx, tenantID, id); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionDeleted,
			TargetType: "live_session", TargetID: id.String(),
		})
	})
}
