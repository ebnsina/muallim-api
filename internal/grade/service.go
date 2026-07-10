package grade

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/platform/database"
)

// Repository is the gradebook's storage.
//
// Every method takes a transaction, never the pool: `app.tenant_id` is bound
// transaction-locally, and a session-level SET on a pooled connection is a
// cross-tenant leak.
type Repository interface {
	// UpsertItem records the assessment, or updates the one already there. Returns
	// the item's id and the course it belongs to, resolved from the lesson.
	UpsertItem(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, s Score) (uuid.UUID, error)
	UpsertEntry(ctx context.Context, tx pgx.Tx, tenantID, itemID uuid.UUID, s Score) error

	Items(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) ([]Item, error)
	EntriesForCourse(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) ([]Entry, error)
	EntriesForLearner(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) ([]Entry, error)
	Learners(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) ([]Learner, error)

	CourseBySlug(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string) (Course, error)

	ScaleByID(ctx context.Context, tx pgx.Tx, tenantID, scaleID uuid.UUID) (Scale, error)
	Scales(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]Scale, error)
	CreateScale(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, s Scale) (Scale, error)
	DeleteScale(ctx context.Context, tx pgx.Tx, tenantID, scaleID uuid.UUID) error
	SetCourseScale(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, scaleID *uuid.UUID) error
}

// Course is as much of a course as the gradebook needs: which one, and how it
// grades.
type Course struct {
	ID    uuid.UUID
	Slug  string
	Title string

	// ScaleID is nil when the course grades by the built-in default.
	ScaleID *uuid.UUID
}

// Learner is somebody enrolled on the course, for the marker's view.
type Learner struct {
	UserID uuid.UUID
	Name   string
	Email  string
}

// Service is the gradebook.
type Service struct {
	db   *database.DB
	repo Repository
}

func NewService(db *database.DB, repo Repository) *Service {
	return &Service{db: db, repo: repo}
}

/*
Record writes a mark, in the transaction that awarded it.

Called by `assess` when a quiz attempt settles and by `assign` when a submission
is marked, through an interface each of them declares. Neither imports this
package, and this package imports neither.

Idempotent, because the callers are. A retried grading job records the same score
again and the gradebook does not gain a second entry.
*/
func (s *Service) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, score Score) error {
	if err := score.validate(); err != nil {
		return err
	}

	itemID, err := s.repo.UpsertItem(ctx, tx, tenantID, score)
	if err != nil {
		return err
	}

	return s.repo.UpsertEntry(ctx, tx, tenantID, itemID, score)
}

// Scale resolves the scale a course grades by: its own, or the built-in default.
func (s *Service) scaleFor(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, course Course) (Scale, error) {
	if course.ScaleID == nil {
		return DefaultScale(), nil
	}

	scale, err := s.repo.ScaleByID(ctx, tx, tenantID, *course.ScaleID)
	if errors.Is(err, ErrNotFound) {
		// The column is `ON DELETE SET NULL`, so this should not happen. If it does,
		// grading by the default beats refusing to render a gradebook.
		return DefaultScale(), nil
	}
	return scale, err
}

// LearnerGrade is one learner's row in a gradebook.
type LearnerGrade struct {
	Learner Learner
	Entries []Entry
	Result  Result
}

// Gradebook is every learner's grade in a course.
type Gradebook struct {
	Course   Course
	Scale    Scale
	Items    []Item
	Learners []LearnerGrade
}

/*
CourseGradebook is the whole course, for whoever may mark it.

Four queries, whatever the size of the class: the course, its items, every entry
against those items, and the learners enrolled. Not one query per learner — a
`database.Counter` test asserts the count across growing fixtures, which is what
makes an N+1 here a build failure rather than a customer's problem.
*/
func (s *Service) CourseGradebook(ctx context.Context, tenantID uuid.UUID, slug string) (Gradebook, error) {
	var book Gradebook

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		course, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug)
		if err != nil {
			return err
		}

		scale, err := s.scaleFor(ctx, tx, tenantID, course)
		if err != nil {
			return err
		}

		items, err := s.repo.Items(ctx, tx, tenantID, course.ID)
		if err != nil {
			return err
		}

		entries, err := s.repo.EntriesForCourse(ctx, tx, tenantID, course.ID)
		if err != nil {
			return err
		}

		learners, err := s.repo.Learners(ctx, tx, tenantID, course.ID)
		if err != nil {
			return err
		}

		// Stitched with a map, not queried per learner.
		byLearner := make(map[uuid.UUID][]Entry, len(learners))
		for _, entry := range entries {
			byLearner[entry.UserID] = append(byLearner[entry.UserID], entry)
		}

		book = Gradebook{Course: course, Scale: scale, Items: items}
		book.Learners = make([]LearnerGrade, 0, len(learners))

		for _, learner := range learners {
			own := byLearner[learner.UserID]
			book.Learners = append(book.Learners, LearnerGrade{
				Learner: learner,
				Entries: own,
				Result:  Summarise(items, own, scale),
			})
		}

		return nil
	})

	return book, err
}

// MyGrades is one learner's own grades in a course.
type MyGrades struct {
	Course  Course
	Scale   Scale
	Items   []Item
	Entries []Entry
	Result  Result
}

// LearnerGrades is what a learner sees: their own marks, and nobody else's.
//
// A learner reads this through their own id, taken from their token. There is no
// path here that takes a user id from a request.
func (s *Service) LearnerGrades(ctx context.Context, tenantID uuid.UUID, slug string, userID uuid.UUID) (MyGrades, error) {
	var grades MyGrades

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		course, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug)
		if err != nil {
			return err
		}

		scale, err := s.scaleFor(ctx, tx, tenantID, course)
		if err != nil {
			return err
		}

		items, err := s.repo.Items(ctx, tx, tenantID, course.ID)
		if err != nil {
			return err
		}

		entries, err := s.repo.EntriesForLearner(ctx, tx, tenantID, course.ID, userID)
		if err != nil {
			return err
		}

		grades = MyGrades{
			Course:  course,
			Scale:   scale,
			Items:   items,
			Entries: entries,
			Result:  Summarise(items, entries, scale),
		}
		return nil
	})

	return grades, err
}

// Scales lists the workspace's scales, with the built-in default first. The
// default is not a row, so it is not in the list twice.
func (s *Service) Scales(ctx context.Context, tenantID uuid.UUID) ([]Scale, error) {
	var scales []Scale

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		stored, err := s.repo.Scales(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		scales = append([]Scale{DefaultScale()}, stored...)
		return nil
	})

	return scales, err
}

// CreateScale adds a workspace scale. A scale that cannot grade every percentage
// exactly once is refused here rather than discovered on a gradebook.
func (s *Service) CreateScale(ctx context.Context, tenantID uuid.UUID, scale Scale) (Scale, error) {
	if err := scale.validate(); err != nil {
		return Scale{}, err
	}

	var created Scale
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		created, err = s.repo.CreateScale(ctx, tx, tenantID, scale)
		return err
	})

	return created, err
}

// DeleteScale removes one. Courses grading by it fall back to the default, which
// is what `ON DELETE SET NULL` on the course's column arranges.
func (s *Service) DeleteScale(ctx context.Context, tenantID, scaleID uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.DeleteScale(ctx, tx, tenantID, scaleID)
	})
}

// SetCourseScale points a course at a scale, or back at the default.
//
// The scale is read inside the transaction before it is assigned. Without that, a
// course could be pointed at a scale in another workspace — the foreign key would
// allow it if the row existed, and RLS is the net rather than the check.
func (s *Service) SetCourseScale(ctx context.Context, tenantID uuid.UUID, slug string, scaleID *uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		course, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug)
		if err != nil {
			return err
		}

		if scaleID != nil {
			if _, err := s.repo.ScaleByID(ctx, tx, tenantID, *scaleID); err != nil {
				return fmt.Errorf("assign scale to %s: %w", slug, err)
			}
		}

		return s.repo.SetCourseScale(ctx, tx, tenantID, course.ID, scaleID)
	})
}
