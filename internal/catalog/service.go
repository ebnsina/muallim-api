package catalog

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/platform/database"
)

// Repository is the persistence contract, declared here by its consumer.
type Repository interface {
	ListCourses(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, p ListParams) ([]Course, error)
	CourseBySlug(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string, includeDrafts bool) (Course, error)
	CurriculumFor(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) ([]Topic, error)
	CreateCourse(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewCourse) (Course, error)
}

// Service holds the business rules and owns transaction boundaries.
type Service struct {
	db        *database.DB
	repo      Repository
	authoring AuthoringRepository
	prereqs   PrerequisiteRepository
	audit     AuditRecorder
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, authoring AuthoringRepository, prereqs PrerequisiteRepository, recorder AuditRecorder) *Service {
	return &Service{db: db, repo: repo, authoring: authoring, prereqs: prereqs, audit: recorder}
}

// ListCourses returns one page of a tenant's courses.
//
// Both reads run in a single read-only transaction. Read-only is not decoration:
// Postgres refuses any write inside it, so "this list endpoint accidentally
// mutates state" stops being something a reviewer has to notice.
func (s *Service) ListCourses(ctx context.Context, tenantID uuid.UUID, p ListParams) (Page, error) {
	p, err := p.normalise()
	if err != nil {
		return Page{}, err
	}

	var page Page
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		courses, err := s.repo.ListCourses(ctx, tx, tenantID, p)
		if err != nil {
			return err
		}

		// The repository fetched one row beyond the page to reveal whether more
		// exist. Trim it before it reaches a client.
		if len(courses) > p.Limit {
			courses = courses[:p.Limit]
			page.HasMore = true
		}

		if page.HasMore && len(courses) > 0 {
			last := courses[len(courses)-1]
			page.NextCursor = cursor{CreatedAt: last.CreatedAt, ID: last.ID}.encode()
		}
		page.Courses = courses
		return nil
	})
	if err != nil {
		return Page{}, err
	}
	return page, nil
}

// Curriculum loads a course with its full topic and lesson tree.
//
// Three queries, always: one for the course, one for its topics, one for every
// lesson of those topics. The count does not grow with the size of the course.
// A test asserts this, so an innocent-looking loop cannot reintroduce an N+1.
//
// includeDrafts is an authorisation decision made by the caller. A reader without
// it gets ErrNotFound for an unpublished course — the same answer as for a course
// that does not exist, because "this exists but you may not see it" is a fact
// about the workspace's plans that strangers have no business learning.
func (s *Service) Curriculum(ctx context.Context, tenantID uuid.UUID, slug string, includeDrafts bool) (Curriculum, error) {
	var out Curriculum

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		course, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug, includeDrafts)
		if err != nil {
			return err
		}

		topics, err := s.repo.CurriculumFor(ctx, tx, tenantID, course.ID)
		if err != nil {
			return err
		}

		out = Curriculum{Course: course, Topics: topics}
		return nil
	})
	if err != nil {
		return Curriculum{}, err
	}
	return out, nil
}

// normalise applies defaults and bounds. It never silently truncates a caller's
// intent without saying so: an out-of-range limit is an error, not a surprise.
func (p ListParams) normalise() (ListParams, error) {
	switch {
	case p.Limit == 0:
		p.Limit = DefaultPageSize
	case p.Limit < 0 || p.Limit > MaxPageSize:
		return p, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidLimit, MaxPageSize)
	}

	return p, nil
}
