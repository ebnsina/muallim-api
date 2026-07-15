package staff

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared by its consumer. Every method
// takes a transaction, never the pool: the tenant is bound transaction-locally.
type Repository interface {
	Create(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewStaff) (Staff, error)
	Roster(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f RosterFilter, after *cursor, limit int) ([]Staff, error)
	ByID(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Staff, error)
	Update(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, p StaffPatch) (Staff, error)
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

// Service holds the staff rules and owns transaction boundaries.
type Service struct {
	db    *database.DB
	repo  Repository
	audit AuditRecorder
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, recorder AuditRecorder) *Service {
	return &Service{db: db, repo: repo, audit: recorder}
}

// Hire adds a staff record.
func (s *Service) Hire(ctx context.Context, tenantID uuid.UUID, n NewStaff, author Author) (Staff, error) {
	if err := n.validate(); err != nil {
		return Staff{}, err
	}
	var st Staff
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		st, err = s.repo.Create(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionHired,
			TargetType: "staff", TargetID: st.ID.String(),
			Metadata: map[string]any{"name": st.FullName, "role": st.Role},
		})
	})
	return st, err
}

// Roster lists staff by name, keyset-paginated, optionally narrowed to a role.
func (s *Service) Roster(ctx context.Context, tenantID uuid.UUID, f RosterFilter, p PageParams) (Page, error) {
	after, err := p.decode()
	if err != nil {
		return Page{}, err
	}
	limit := p.clamp()

	var page Page
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.repo.Roster(ctx, tx, tenantID, f, after, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			page.HasMore = true
			page.NextCursor = encodeCursor(rows[limit-1])
			rows = rows[:limit]
		}
		page.Staff = rows
		return nil
	})
	return page, err
}

// Member reads one staff record.
func (s *Service) Member(ctx context.Context, tenantID, id uuid.UUID) (Staff, error) {
	var st Staff
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		st, err = s.repo.ByID(ctx, tx, tenantID, id)
		return err
	})
	return st, err
}

// Edit changes a staff record's details.
func (s *Service) Edit(ctx context.Context, tenantID, id uuid.UUID, p StaffPatch) (Staff, error) {
	if err := p.validate(); err != nil {
		return Staff{}, err
	}
	var st Staff
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		st, err = s.repo.Update(ctx, tx, tenantID, id, p)
		return err
	})
	return st, err
}

// Remove deletes a staff record.
func (s *Service) Remove(ctx context.Context, tenantID, id uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.Delete(ctx, tx, tenantID, id)
	})
}
