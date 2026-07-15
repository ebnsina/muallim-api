package catalog

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/blob"
	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared here by its consumer.
type Repository interface {
	ListCourses(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, p ListParams) ([]Course, error)
	CourseBySlug(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string, includeDrafts bool) (Course, error)
	CurriculumFor(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) ([]Topic, error)
	CreateCourse(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewCourse, createdBy uuid.UUID) (Course, error)
	UpdateCourse(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string, p CoursePatch) (Course, error)

	CreateAnnouncement(ctx context.Context, tx pgx.Tx, tenantID, courseID, authorID uuid.UUID, title, body string) (Announcement, error)
	Announcements(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) ([]Announcement, error)
	DeleteAnnouncement(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (bool, error)
}

// AnnouncementNotifier queues, on the caller's transaction, the fan-out that
// tells a course's enrolled learners a notice was posted. Declared here, by its
// consumer, and satisfied in cmd by the notify service's enqueuer — the fan-out
// is a job because a busy course has too many learners to notify inline. A nil
// notifier posts the announcement and tells no one, which is what the spec-only
// build passes.
type AnnouncementNotifier interface {
	NotifyAnnouncement(ctx context.Context, tx pgx.Tx, tenantID, courseID, announcementID uuid.UUID, title, body, link string) error
}

// Service holds the business rules and owns transaction boundaries.
type Service struct {
	db        *database.DB
	repo      Repository
	authoring AuthoringRepository
	prereqs   PrerequisiteRepository
	audit     AuditRecorder
	video     VideoResolver
	announce  AnnouncementNotifier
	store     blob.Store
}

// NewService returns a Service. The store holds course thumbnails; a nil-behaving
// (Unconfigured) store refuses those uploads.
func NewService(db *database.DB, repo Repository, authoring AuthoringRepository, prereqs PrerequisiteRepository, recorder AuditRecorder, video VideoResolver, announce AnnouncementNotifier, store blob.Store) *Service {
	return &Service{db: db, repo: repo, authoring: authoring, prereqs: prereqs, audit: recorder, video: video, announce: announce, store: store}
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

// PostAnnouncement pins a notice to a course. The course is resolved with drafts
// visible, because the caller holds course:write and may post to a course before
// they publish it. Title and body are trimmed and bounded; empty is refused.
func (s *Service) PostAnnouncement(ctx context.Context, tenantID uuid.UUID, slug string, authorID uuid.UUID, title, body string) (Announcement, error) {
	title = strings.TrimSpace(title)
	body = strings.TrimSpace(body)
	if title == "" || body == "" || len(title) > MaxAnnouncementTitle || len(body) > MaxAnnouncementBody {
		return Announcement{}, ErrInvalidAnnouncement
	}

	var created Announcement
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		course, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug, true)
		if err != nil {
			return err
		}
		created, err = s.repo.CreateAnnouncement(ctx, tx, tenantID, course.ID, authorID, title, body)
		if err != nil {
			return err
		}

		// Notify the course's enrolled learners, on this transaction. The fan-out is
		// a job, enqueued here so it exists iff the announcement committed — post the
		// notice and enqueue nothing, or roll both back together.
		if s.announce != nil {
			link := "/courses/" + slug
			return s.announce.NotifyAnnouncement(ctx, tx, tenantID, course.ID, created.ID, title, body, link)
		}
		return nil
	})

	return created, err
}

// Announcements lists a course's notices, newest first. Whether a reader may see
// them follows the course's own visibility: a published course's notices are for
// anyone who can reach it, a draft's for the author, and `includeDrafts` carries
// that decision from the transport layer, never from a request.
func (s *Service) Announcements(ctx context.Context, tenantID uuid.UUID, slug string, includeDrafts bool) ([]Announcement, error) {
	var announcements []Announcement

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		course, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug, includeDrafts)
		if err != nil {
			return err
		}
		announcements, err = s.repo.Announcements(ctx, tx, tenantID, course.ID)
		return err
	})

	return announcements, err
}

// DeleteAnnouncement removes a notice. Scoped to the workspace by id; a notice
// that is not there — or belongs to another tenant, which is the same thing under
// RLS — is ErrNotFound.
func (s *Service) DeleteAnnouncement(ctx context.Context, tenantID, id uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		found, err := s.repo.DeleteAnnouncement(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		return nil
	})
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

	// A blank search filters nothing; the query treats an empty string as no
	// filter, so trimming it to empty is the same as not having asked.
	p.Search = strings.TrimSpace(p.Search)

	// A difficulty the schema does not recognise is dropped rather than refused: a
	// stale bookmark or a typo shows the whole catalogue, not an error page.
	if !validDifficulties[p.Difficulty] {
		p.Difficulty = ""
	}

	return p, nil
}

// The levels the courses table's CHECK allows. A filter outside them can match
// nothing, so it is treated as no filter at all.
var validDifficulties = map[string]bool{
	"beginner": true, "intermediate": true, "advanced": true, "expert": true,
}
