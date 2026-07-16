package bundle

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared by its consumer. Every method
// takes a transaction, never the pool: the tenant is bound transaction-locally.
type Repository interface {
	Create(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewBundle) (Bundle, error)
	List(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *cursor, limit int) ([]Bundle, error)
	BySlug(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string) (Bundle, error)
	ByID(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Bundle, error)
	Update(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string, p BundlePatch) (Bundle, error)
	Delete(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string) error
	SetCourses(ctx context.Context, tx pgx.Tx, tenantID, bundleID uuid.UUID, courseIDs []uuid.UUID) error
	// CoursesFor loads the ordered courses of every named bundle in one query, keyed
	// by bundle id — the batched load that keeps a bundle's Get free of an N+1.
	CoursesFor(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, bundleIDs []uuid.UUID) (map[uuid.UUID][]CourseRef, error)
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

// Service holds the bundle rules and owns transaction boundaries.
type Service struct {
	db    *database.DB
	repo  Repository
	audit AuditRecorder
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, recorder AuditRecorder) *Service {
	return &Service{db: db, repo: repo, audit: recorder}
}

// Create defines a bundle. A duplicate slug is a conflict, not a 500.
func (s *Service) Create(ctx context.Context, tenantID uuid.UUID, n NewBundle, author Author) (Bundle, error) {
	if err := n.validate(); err != nil {
		return Bundle{}, err
	}
	var b Bundle
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		b, err = s.repo.Create(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionCreated,
			TargetType: "bundle", TargetID: b.ID.String(),
			Metadata: map[string]any{"slug": b.Slug, "name": b.Name, "price_amount": b.PriceAmount, "currency": b.Currency},
		})
	})
	return b, err
}

// List returns one page of bundles, newest first, keyset-paginated.
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
		page.Bundles = rows
		return nil
	})
	return page, err
}

// Get loads a bundle by slug together with its ordered courses. Two queries: the
// bundle, then its course refs batched with `= ANY` — no N+1.
func (s *Service) Get(ctx context.Context, tenantID uuid.UUID, slug string) (Bundle, error) {
	var b Bundle
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		b, err = s.repo.BySlug(ctx, tx, tenantID, slug)
		if err != nil {
			return err
		}
		byBundle, err := s.repo.CoursesFor(ctx, tx, tenantID, []uuid.UUID{b.ID})
		if err != nil {
			return err
		}
		b.Courses = byBundle[b.ID]
		return nil
	})
	return b, err
}

// Update changes a bundle's name, description, or price.
func (s *Service) Update(ctx context.Context, tenantID uuid.UUID, slug string, p BundlePatch, author Author) (Bundle, error) {
	if err := p.validate(); err != nil {
		return Bundle{}, err
	}
	var b Bundle
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		b, err = s.repo.Update(ctx, tx, tenantID, slug, p)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionUpdated,
			TargetType: "bundle", TargetID: b.ID.String(),
			Metadata: map[string]any{"slug": b.Slug},
		})
	})
	return b, err
}

// Delete removes a bundle. Its bundle_courses rows cascade; the courses themselves
// are untouched.
func (s *Service) Delete(ctx context.Context, tenantID uuid.UUID, slug string, author Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.repo.Delete(ctx, tx, tenantID, slug); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionDeleted,
			TargetType: "bundle", TargetID: slug,
		})
	})
}

// SetCourses replaces a bundle's course list with the submitted order, positions
// dense from the list index. Returns the bundle reloaded with its new courses.
func (s *Service) SetCourses(ctx context.Context, tenantID, bundleID uuid.UUID, courseIDs []uuid.UUID, author Author) (Bundle, error) {
	if len(courseIDs) > MaxCourses {
		return Bundle{}, ErrInvalid
	}
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// ByID confirms the bundle exists (404 rather than a foreign-key 500) before
		// the replace touches bundle_courses.
		if _, err := s.repo.ByID(ctx, tx, tenantID, bundleID); err != nil {
			return err
		}
		if err := s.repo.SetCourses(ctx, tx, tenantID, bundleID, courseIDs); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionCoursesSet,
			TargetType: "bundle", TargetID: bundleID.String(),
			Metadata: map[string]any{"count": len(courseIDs)},
		})
	})
	if err != nil {
		return Bundle{}, err
	}
	// Reload so the returned value reflects the committed state, with its courses.
	return s.getByID(ctx, tenantID, bundleID)
}

func (s *Service) getByID(ctx context.Context, tenantID, bundleID uuid.UUID) (Bundle, error) {
	var b Bundle
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		b, err = s.repo.ByID(ctx, tx, tenantID, bundleID)
		if err != nil {
			return err
		}
		byBundle, err := s.repo.CoursesFor(ctx, tx, tenantID, []uuid.UUID{b.ID})
		if err != nil {
			return err
		}
		b.Courses = byBundle[b.ID]
		return nil
	})
	return b, err
}

// CourseIDs returns the ids of the courses in a bundle, in order. The coordinator
// calls this to enrol a grantee in each — one read query.
func (s *Service) CourseIDs(ctx context.Context, tenantID, bundleID uuid.UUID) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		byBundle, err := s.repo.CoursesFor(ctx, tx, tenantID, []uuid.UUID{bundleID})
		if err != nil {
			return err
		}
		refs := byBundle[bundleID]
		ids = make([]uuid.UUID, 0, len(refs))
		for _, r := range refs {
			ids = append(ids, r.CourseID)
		}
		return nil
	})
	return ids, err
}
