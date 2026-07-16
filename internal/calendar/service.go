package calendar

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared by its consumer. Every method
// takes a transaction, never the pool: the tenant is bound transaction-locally.
type Repository interface {
	Create(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewEvent) (Event, error)
	List(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f EventFilter, after *cursor, limit int) ([]Event, error)
	Update(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, p EventPatch) (Event, error)
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

// Service holds the calendar rules and owns transaction boundaries.
type Service struct {
	db    *database.DB
	repo  Repository
	audit AuditRecorder
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, recorder AuditRecorder) *Service {
	return &Service{db: db, repo: repo, audit: recorder}
}

// CreateEvent adds an entry to the calendar.
func (s *Service) CreateEvent(ctx context.Context, tenantID uuid.UUID, n NewEvent, author Author) (Event, error) {
	if err := n.validate(); err != nil {
		return Event{}, err
	}
	var ev Event
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ev, err = s.repo.Create(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionCreated,
			TargetType: "calendar_event", TargetID: ev.ID.String(),
			Metadata: map[string]any{"title": ev.Title, "kind": ev.Kind},
		})
	})
	return ev, err
}

// ListEvents lists the calendar, newest first, keyset-paginated. Filters by kind
// and to a date window on the start date.
func (s *Service) ListEvents(ctx context.Context, tenantID uuid.UUID, f EventFilter, p PageParams) (Page, error) {
	after, err := p.decode()
	if err != nil {
		return Page{}, err
	}
	limit := p.clamp()

	var page Page
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.repo.List(ctx, tx, tenantID, f, after, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			page.HasMore = true
			page.NextCursor = encodeCursor(rows[limit-1])
			rows = rows[:limit]
		}
		page.Events = rows
		return nil
	})
	return page, err
}

// UpdateEvent changes an event's details.
func (s *Service) UpdateEvent(ctx context.Context, tenantID, id uuid.UUID, p EventPatch) (Event, error) {
	if err := p.validate(); err != nil {
		return Event{}, err
	}
	var ev Event
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ev, err = s.repo.Update(ctx, tx, tenantID, id, p)
		return err
	})
	return ev, err
}

// DeleteEvent removes an event from the calendar.
func (s *Service) DeleteEvent(ctx context.Context, tenantID, id uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.Delete(ctx, tx, tenantID, id)
	})
}
