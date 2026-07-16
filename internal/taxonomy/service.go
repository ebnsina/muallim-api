package taxonomy

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared by its consumer. Every method
// takes a transaction, never the pool: the tenant is bound transaction-locally.
type Repository interface {
	CreateCategory(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewTerm) (Category, error)
	Categories(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *cursor, limit int) ([]Category, error)
	DeleteCategory(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error

	CreateTag(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewTerm) (Tag, error)
	Tags(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *cursor, limit int) ([]Tag, error)
	DeleteTag(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error

	SetCourseCategory(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, categoryID *uuid.UUID) error
	CategoryForCourse(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) (*Category, error)
	SetCourseTags(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, tagIDs []uuid.UUID) error
	TagsForCourse(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) ([]Tag, error)

	CourseIDsInCategory(ctx context.Context, tx pgx.Tx, tenantID, categoryID uuid.UUID) ([]uuid.UUID, error)
	CourseIDsWithTag(ctx context.Context, tx pgx.Tx, tenantID, tagID uuid.UUID) ([]uuid.UUID, error)
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

// Author identifies who did something, for the audit trail.
type Author struct {
	UserID uuid.UUID
}

// Service holds the taxonomy rules and owns transaction boundaries.
type Service struct {
	db    *database.DB
	repo  Repository
	audit AuditRecorder
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, recorder AuditRecorder) *Service {
	return &Service{db: db, repo: repo, audit: recorder}
}

// CreateCategory adds a catalogue section. A slug clash is ErrDuplicate.
func (s *Service) CreateCategory(ctx context.Context, tenantID uuid.UUID, n NewTerm, author Author) (Category, error) {
	if err := n.validate(); err != nil {
		return Category{}, err
	}
	var c Category
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		c, err = s.repo.CreateCategory(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionCategoryCreated,
			TargetType: "course_category", TargetID: c.ID.String(),
			Metadata: map[string]any{"name": c.Name, "slug": c.Slug},
		})
	})
	return c, err
}

// ListCategories lists catalogue sections by name, keyset-paginated.
func (s *Service) ListCategories(ctx context.Context, tenantID uuid.UUID, p PageParams) (CategoryPage, error) {
	after, err := p.decode()
	if err != nil {
		return CategoryPage{}, err
	}
	limit := p.clamp()

	var page CategoryPage
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.repo.Categories(ctx, tx, tenantID, after, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			page.HasMore = true
			page.NextCursor = encodeCategoryCursor(rows[limit-1])
			rows = rows[:limit]
		}
		page.Categories = rows
		return nil
	})
	return page, err
}

// DeleteCategory removes a category; its course links cascade away.
func (s *Service) DeleteCategory(ctx context.Context, tenantID, id uuid.UUID, author Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.repo.DeleteCategory(ctx, tx, tenantID, id); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionCategoryDeleted,
			TargetType: "course_category", TargetID: id.String(),
		})
	})
}

// CreateTag adds a cross-cutting label. A slug clash is ErrDuplicate.
func (s *Service) CreateTag(ctx context.Context, tenantID uuid.UUID, n NewTerm, author Author) (Tag, error) {
	if err := n.validate(); err != nil {
		return Tag{}, err
	}
	var t Tag
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		t, err = s.repo.CreateTag(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionTagCreated,
			TargetType: "course_tag", TargetID: t.ID.String(),
			Metadata: map[string]any{"name": t.Name, "slug": t.Slug},
		})
	})
	return t, err
}

// ListTags lists labels by name, keyset-paginated.
func (s *Service) ListTags(ctx context.Context, tenantID uuid.UUID, p PageParams) (TagPage, error) {
	after, err := p.decode()
	if err != nil {
		return TagPage{}, err
	}
	limit := p.clamp()

	var page TagPage
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.repo.Tags(ctx, tx, tenantID, after, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			page.HasMore = true
			page.NextCursor = encodeTagCursor(rows[limit-1])
			rows = rows[:limit]
		}
		page.Tags = rows
		return nil
	})
	return page, err
}

// DeleteTag removes a tag; its course links cascade away.
func (s *Service) DeleteTag(ctx context.Context, tenantID, id uuid.UUID, author Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.repo.DeleteTag(ctx, tx, tenantID, id); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionTagDeleted,
			TargetType: "course_tag", TargetID: id.String(),
		})
	})
}

// SetCourseCategory files a course under a category, or clears it when categoryID
// is nil. Replaces any existing category — a course sits in at most one.
func (s *Service) SetCourseCategory(ctx context.Context, tenantID, courseID uuid.UUID, categoryID *uuid.UUID, author Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.repo.SetCourseCategory(ctx, tx, tenantID, courseID, categoryID); err != nil {
			return err
		}
		meta := map[string]any{"course_id": courseID.String()}
		if categoryID != nil {
			meta["category_id"] = categoryID.String()
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionCourseCategory,
			TargetType: "course", TargetID: courseID.String(), Metadata: meta,
		})
	})
}

// CourseCategory returns the category a course is filed under, or nil.
func (s *Service) CourseCategory(ctx context.Context, tenantID, courseID uuid.UUID) (*Category, error) {
	var c *Category
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		c, err = s.repo.CategoryForCourse(ctx, tx, tenantID, courseID)
		return err
	})
	return c, err
}

// SetCourseTags replaces a course's tags with exactly the given set (empty clears
// them). One statement — the delete and insert are a single CTE.
func (s *Service) SetCourseTags(ctx context.Context, tenantID, courseID uuid.UUID, tagIDs []uuid.UUID, author Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.repo.SetCourseTags(ctx, tx, tenantID, courseID, dedupe(tagIDs)); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionCourseTags,
			TargetType: "course", TargetID: courseID.String(),
			Metadata: map[string]any{"course_id": courseID.String(), "count": len(tagIDs)},
		})
	})
}

// CourseTags returns the tags a course carries, by name.
func (s *Service) CourseTags(ctx context.Context, tenantID, courseID uuid.UUID) ([]Tag, error) {
	var tags []Tag
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		tags, err = s.repo.TagsForCourse(ctx, tx, tenantID, courseID)
		return err
	})
	return tags, err
}

// CourseIDsInCategory returns the ids of the courses filed under a category, for
// the catalogue to filter.
func (s *Service) CourseIDsInCategory(ctx context.Context, tenantID, categoryID uuid.UUID) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ids, err = s.repo.CourseIDsInCategory(ctx, tx, tenantID, categoryID)
		return err
	})
	return ids, err
}

// CourseIDsWithTag returns the ids of the courses carrying a tag, for the
// catalogue to filter.
func (s *Service) CourseIDsWithTag(ctx context.Context, tenantID, tagID uuid.UUID) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ids, err = s.repo.CourseIDsWithTag(ctx, tx, tenantID, tagID)
		return err
	})
	return ids, err
}

// CourseIDBySlug resolves a course slug to its id, ErrNotFound if unknown. The
// HTTP layer uses it to keep the domain self-contained without importing catalog.
func (s *Service) CourseIDBySlug(ctx context.Context, tenantID uuid.UUID, slug string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = s.repo.CourseIDBySlug(ctx, tx, tenantID, slug)
		return err
	})
	return id, err
}

// CourseTaxonomyBySlug resolves a course by slug and returns its category and tags
// in one read transaction.
func (s *Service) CourseTaxonomyBySlug(ctx context.Context, tenantID uuid.UUID, slug string) (*Category, []Tag, error) {
	var cat *Category
	var tags []Tag
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		courseID, err := s.repo.CourseIDBySlug(ctx, tx, tenantID, slug)
		if err != nil {
			return err
		}
		cat, err = s.repo.CategoryForCourse(ctx, tx, tenantID, courseID)
		if err != nil {
			return err
		}
		tags, err = s.repo.TagsForCourse(ctx, tx, tenantID, courseID)
		return err
	})
	return cat, tags, err
}

// SetCourseTaxonomyBySlug resolves a course by slug and sets its category and tags
// together, in one transaction, returning the resulting taxonomy.
func (s *Service) SetCourseTaxonomyBySlug(ctx context.Context, tenantID uuid.UUID, slug string, categoryID *uuid.UUID, tagIDs []uuid.UUID, author Author) (*Category, []Tag, error) {
	var cat *Category
	var tags []Tag
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		courseID, err := s.repo.CourseIDBySlug(ctx, tx, tenantID, slug)
		if err != nil {
			return err
		}
		if err := s.repo.SetCourseCategory(ctx, tx, tenantID, courseID, categoryID); err != nil {
			return err
		}
		if err := s.repo.SetCourseTags(ctx, tx, tenantID, courseID, dedupe(tagIDs)); err != nil {
			return err
		}
		if err := s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionCourseTags,
			TargetType: "course", TargetID: courseID.String(),
			Metadata: map[string]any{"course_id": courseID.String(), "tag_count": len(tagIDs)},
		}); err != nil {
			return err
		}
		cat, err = s.repo.CategoryForCourse(ctx, tx, tenantID, courseID)
		if err != nil {
			return err
		}
		tags, err = s.repo.TagsForCourse(ctx, tx, tenantID, courseID)
		return err
	})
	return cat, tags, err
}

// dedupe returns the ids with duplicates removed, order preserved.
func dedupe(ids []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
