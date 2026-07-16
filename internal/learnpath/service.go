package learnpath

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared by its consumer. Every method
// takes a transaction, never the pool: the tenant is bound transaction-locally.
type Repository interface {
	Create(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewPath) (Path, error)
	List(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f PathFilter, after *cursor, limit int) ([]Path, error)
	BySlug(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string) (Path, error)
	Update(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string, p PathPatch) (Path, error)
	Delete(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string) error

	// CourseRefs returns a path's courses in order, in one query.
	CourseRefs(ctx context.Context, tx pgx.Tx, tenantID, pathID uuid.UUID) ([]CourseRef, error)
	// SetCourses replaces a path's whole membership with the ordered list.
	SetCourses(ctx context.Context, tx pgx.Tx, tenantID, pathID uuid.UUID, order []uuid.UUID) error
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

// Service holds the learning-path rules and owns transaction boundaries.
type Service struct {
	db    *database.DB
	repo  Repository
	audit AuditRecorder
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, recorder AuditRecorder) *Service {
	return &Service{db: db, repo: repo, audit: recorder}
}

// Create adds a draft path. Publishing is a later Update.
func (s *Service) Create(ctx context.Context, tenantID uuid.UUID, n NewPath, author Author) (Path, error) {
	if err := n.validate(); err != nil {
		return Path{}, err
	}
	var p Path
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		p, err = s.repo.Create(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionPathCreated,
			TargetType: "learning_path", TargetID: p.ID.String(),
			Metadata: map[string]any{"slug": p.Slug, "title": p.Title},
		})
	})
	return p, err
}

// List lists paths, newest first, keyset-paginated, optionally by status.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID, f PathFilter, pp PageParams) (Page, error) {
	after, err := pp.decode()
	if err != nil {
		return Page{}, err
	}
	limit := pp.clamp()

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
		page.Paths = rows
		return nil
	})
	return page, err
}

// Get loads one path with its course ids in order. One extra query loads the
// courses, whatever their number — no per-course round trip.
func (s *Service) Get(ctx context.Context, tenantID uuid.UUID, slug string) (Path, error) {
	var p Path
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		p, err = s.repo.BySlug(ctx, tx, tenantID, slug)
		if err != nil {
			return err
		}
		refs, err := s.repo.CourseRefs(ctx, tx, tenantID, p.ID)
		if err != nil {
			return err
		}
		p.Courses = flatten(refs)
		return nil
	})
	return p, err
}

// Update applies a patch — title, description, and/or the draft↔published status.
func (s *Service) Update(ctx context.Context, tenantID uuid.UUID, slug string, patch PathPatch, author Author) (Path, error) {
	if err := patch.validate(); err != nil {
		return Path{}, err
	}
	var p Path
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		p, err = s.repo.Update(ctx, tx, tenantID, slug, patch)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionPathUpdated,
			TargetType: "learning_path", TargetID: p.ID.String(),
			Metadata: map[string]any{"status": p.Status},
		})
	})
	return p, err
}

// SetCourses replaces a path's whole ordered course list. The list must name each
// course exactly once; a repeat is ErrIncompleteOrder. Three statements for any
// number of courses: existence check, clear, and one unnest-ordinality insert.
func (s *Service) SetCourses(ctx context.Context, tenantID, pathID uuid.UUID, order []uuid.UUID, author Author) error {
	if err := dedupe(order); err != nil {
		return err
	}
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.repo.SetCourses(ctx, tx, tenantID, pathID, order); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionCoursesSet,
			TargetType: "learning_path", TargetID: pathID.String(),
			Metadata: map[string]any{"count": len(order)},
		})
	})
}

// CourseIDs returns a path's course ids in order — what the coordinator maps
// per-course progress onto. One query.
func (s *Service) CourseIDs(ctx context.Context, tenantID, pathID uuid.UUID) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		refs, err := s.repo.CourseRefs(ctx, tx, tenantID, pathID)
		if err != nil {
			return err
		}
		ids = flatten(refs)
		return nil
	})
	return ids, err
}

// Delete removes a path and its course rows (cascade).
func (s *Service) Delete(ctx context.Context, tenantID uuid.UUID, slug string, author Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.repo.Delete(ctx, tx, tenantID, slug); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionPathDeleted,
			TargetType: "learning_path", TargetID: slug,
			Metadata: map[string]any{"slug": slug},
		})
	})
}

func flatten(refs []CourseRef) []uuid.UUID {
	ids := make([]uuid.UUID, len(refs))
	for i, r := range refs {
		ids[i] = r.CourseID
	}
	return ids
}
