package admissions

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared by its consumer. Every method
// takes a transaction, never the pool: the tenant is bound transaction-locally.
type Repository interface {
	Create(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewApplication) (Application, error)
	List(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f Filter, after *cursor, limit int) ([]Application, error)
	ByID(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Application, error)
	Decide(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, from, to string) (Application, error)
	Admit(ctx context.Context, tx pgx.Tx, tenantID, id, studentID uuid.UUID) (Application, error)
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

// Service holds the admissions rules and owns transaction boundaries.
type Service struct {
	db    *database.DB
	repo  Repository
	audit AuditRecorder
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, recorder AuditRecorder) *Service {
	return &Service{db: db, repo: repo, audit: recorder}
}

// Submit records a new application in the pending queue.
func (s *Service) Submit(ctx context.Context, tenantID uuid.UUID, n NewApplication, author Author) (Application, error) {
	if err := n.validate(); err != nil {
		return Application{}, err
	}
	var app Application
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		app, err = s.repo.Create(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionSubmitted,
			TargetType: "admission_application", TargetID: app.ID.String(),
			Metadata: map[string]any{"applicant": app.ApplicantName},
		})
	})
	return app, err
}

// List lists applications, newest first, keyset-paginated, optionally by status.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID, f Filter, p PageParams) (Page, error) {
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
		page.Applications = rows
		return nil
	})
	return page, err
}

// Get reads one application.
func (s *Service) Get(ctx context.Context, tenantID, id uuid.UUID) (Application, error) {
	var app Application
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		app, err = s.repo.ByID(ctx, tx, tenantID, id)
		return err
	})
	return app, err
}

// Accept marks a pending application accepted. The `WHERE status = 'pending'` guard
// makes a double submission harmless — the second finds nothing pending.
func (s *Service) Accept(ctx context.Context, tenantID, id uuid.UUID, author Author) (Application, error) {
	return s.decide(ctx, tenantID, id, StatusPending, StatusAccepted, ActionAccepted, author)
}

// Reject marks a pending application rejected.
func (s *Service) Reject(ctx context.Context, tenantID, id uuid.UUID, author Author) (Application, error) {
	return s.decide(ctx, tenantID, id, StatusPending, StatusRejected, ActionRejected, author)
}

func (s *Service) decide(ctx context.Context, tenantID, id uuid.UUID, from, to, action string, author Author) (Application, error) {
	var app Application
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		app, err = s.repo.Decide(ctx, tx, tenantID, id, from, to)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: action,
			TargetType: "admission_application", TargetID: app.ID.String(),
		})
	})
	return app, err
}

// MarkAdmitted records that an accepted application became a student. It is the
// admissions half of the admit orchestration: the coordinator creates the student
// (a cross-domain step) and calls this to close the application in the same tenant.
// The `WHERE status = 'accepted'` guard means only an accepted application can be
// admitted, and admitting one twice is refused (ErrNotPending).
func (s *Service) MarkAdmitted(ctx context.Context, tenantID, id, studentID uuid.UUID, author Author) (Application, error) {
	var app Application
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		app, err = s.repo.Admit(ctx, tx, tenantID, id, studentID)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionAdmitted,
			TargetType: "admission_application", TargetID: app.ID.String(),
			Metadata: map[string]any{"student_id": studentID.String()},
		})
	})
	return app, err
}
