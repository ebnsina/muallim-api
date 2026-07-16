package coursebuild

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared by its consumer. Every method
// takes a transaction, never the pool: the tenant is bound transaction-locally.
type Repository interface {
	Create(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewBlueprint) (Blueprint, error)
	List(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *cursor, limit int) ([]Blueprint, error)
	Get(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Blueprint, error)
	Update(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, p BlueprintPatch) (Blueprint, error)
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

// Service holds the course-builder rules and owns transaction boundaries.
type Service struct {
	db    *database.DB
	repo  Repository
	audit AuditRecorder
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, recorder AuditRecorder) *Service {
	return &Service{db: db, repo: repo, audit: recorder}
}

// Create saves a new blueprint after validating its structure.
func (s *Service) Create(ctx context.Context, tenantID uuid.UUID, n NewBlueprint, author Author) (Blueprint, error) {
	if err := n.validate(); err != nil {
		return Blueprint{}, err
	}
	var b Blueprint
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		b, err = s.repo.Create(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionCreated,
			TargetType: "course_blueprint", TargetID: b.ID.String(),
			Metadata: map[string]any{"name": b.Name},
		})
	})
	return b, err
}

// List lists blueprints, newest first, keyset-paginated.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID, p PageParams) (Page, error) {
	after, err := p.decode()
	if err != nil {
		return Page{}, err
	}
	limit := p.clamp()

	var page Page
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.repo.List(ctx, tx, tenantID, after, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			page.HasMore = true
			page.NextCursor = encodeCursor(rows[limit-1])
			rows = rows[:limit]
		}
		page.Blueprints = rows
		return nil
	})
	return page, err
}

// Get returns one blueprint, or ErrNotFound.
func (s *Service) Get(ctx context.Context, tenantID, id uuid.UUID) (Blueprint, error) {
	var b Blueprint
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		b, err = s.repo.Get(ctx, tx, tenantID, id)
		return err
	})
	return b, err
}

// Update changes a blueprint's name, description and/or structure. A nil field is
// left as it stands.
func (s *Service) Update(ctx context.Context, tenantID, id uuid.UUID, p BlueprintPatch, author Author) (Blueprint, error) {
	if err := p.validate(); err != nil {
		return Blueprint{}, err
	}
	var b Blueprint
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		b, err = s.repo.Update(ctx, tx, tenantID, id, p)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionUpdated,
			TargetType: "course_blueprint", TargetID: b.ID.String(),
			Metadata: map[string]any{"name": b.Name},
		})
	})
	return b, err
}

// Delete removes a blueprint, or reports ErrNotFound if there was none to remove.
func (s *Service) Delete(ctx context.Context, tenantID, id uuid.UUID, author Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.repo.Delete(ctx, tx, tenantID, id); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionDeleted,
			TargetType: "course_blueprint", TargetID: id.String(),
		})
	})
}
