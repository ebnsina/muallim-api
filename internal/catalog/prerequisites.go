package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Prerequisite errors.
var (
	// ErrPrerequisiteCycle is returned when an edge would let a course require
	// itself, directly or through any chain of other courses. A cycle is not a
	// hard course to finish; it is a course nobody can ever start.
	ErrPrerequisiteCycle = errors.New("catalog: a course cannot require itself, directly or indirectly")

	// ErrPrerequisiteExists is returned when the edge is already there.
	ErrPrerequisiteExists = errors.New("catalog: that course is already a prerequisite")
)

// Audit actions for the prerequisite graph.
const (
	ActionPrerequisiteAdded   = "course.prerequisite_added"
	ActionPrerequisiteRemoved = "course.prerequisite_removed"
)

// PrerequisiteRepository persists the prerequisite graph. Declared here by its
// consumer.
type PrerequisiteRepository interface {
	AddPrerequisite(ctx context.Context, tx pgx.Tx, tenantID, courseID, requiresID uuid.UUID) error
	RemovePrerequisite(ctx context.Context, tx pgx.Tx, tenantID, courseID, requiresID uuid.UUID) (bool, error)
	Prerequisites(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) ([]Course, error)
	WouldCycle(ctx context.Context, tx pgx.Tx, tenantID, courseID, requiresID uuid.UUID) (bool, error)
}

// Prerequisites lists the courses a course requires.
//
// Visibility follows the course: a reader who may not see the course gets
// ErrNotFound, not an empty list. A prerequisite may itself be a draft, and it is
// listed either way — a learner who cannot enrol needs to know why, and the title
// of an unreleased course is the least of what the enrolment refusal reveals.
// The course itself is returned alongside, so the transport layer can decide
// cacheability from its status rather than from who asked — the same rule the
// curriculum endpoint follows, and for the same reason: an author fetching a
// published course should still take the shared path.
func (s *Service) Prerequisites(ctx context.Context, tenantID uuid.UUID, slug string, includeDrafts bool) (Course, []Course, error) {
	var (
		course Course
		out    []Course
	)

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		course, err = s.repo.CourseBySlug(ctx, tx, tenantID, slug, includeDrafts)
		if err != nil {
			return err
		}

		out, err = s.prereqs.Prerequisites(ctx, tx, tenantID, course.ID)
		return err
	})
	if err != nil {
		return Course{}, nil, err
	}
	return course, out, nil
}

// AddPrerequisite makes `requires` a prerequisite of `course`.
//
// Both are named by slug, because that is what an author knows and what a URL
// carries. Either being absent is ErrNotFound: an author who cannot see a course
// must not learn it exists by trying to depend on it.
func (s *Service) AddPrerequisite(ctx context.Context, tenantID uuid.UUID, courseSlug, requiresSlug string, a Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		course, err := s.repo.CourseBySlug(ctx, tx, tenantID, courseSlug, true)
		if err != nil {
			return err
		}
		requires, err := s.repo.CourseBySlug(ctx, tx, tenantID, requiresSlug, true)
		if err != nil {
			return err
		}

		if course.ID == requires.ID {
			return ErrPrerequisiteCycle
		}

		// Asked of the database, inside this transaction, because the answer depends
		// on every other edge and two authors adding opposite edges at once would
		// each find their own harmless in isolation.
		cycle, err := s.prereqs.WouldCycle(ctx, tx, tenantID, course.ID, requires.ID)
		if err != nil {
			return err
		}
		if cycle {
			return fmt.Errorf("%w: %s already depends on %s", ErrPrerequisiteCycle, requiresSlug, courseSlug)
		}

		if err := s.prereqs.AddPrerequisite(ctx, tx, tenantID, course.ID, requires.ID); err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &a.UserID, Action: ActionPrerequisiteAdded,
			TargetType: "course", TargetID: course.ID.String(),
			IP: a.IP, UserAgent: a.UserAgent,
			Metadata: map[string]any{"course": courseSlug, "requires": requiresSlug},
		})
	})
}

// RemovePrerequisite drops the edge. Removing one that is not there is
// ErrNotFound, not success: an author who typed the wrong slug should hear about
// it.
func (s *Service) RemovePrerequisite(ctx context.Context, tenantID uuid.UUID, courseSlug, requiresSlug string, a Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		course, err := s.repo.CourseBySlug(ctx, tx, tenantID, courseSlug, true)
		if err != nil {
			return err
		}
		requires, err := s.repo.CourseBySlug(ctx, tx, tenantID, requiresSlug, true)
		if err != nil {
			return err
		}

		removed, err := s.prereqs.RemovePrerequisite(ctx, tx, tenantID, course.ID, requires.ID)
		if err != nil {
			return err
		}
		if !removed {
			return ErrNotFound
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &a.UserID, Action: ActionPrerequisiteRemoved,
			TargetType: "course", TargetID: course.ID.String(),
			IP: a.IP, UserAgent: a.UserAgent,
			Metadata: map[string]any{"course": courseSlug, "requires": requiresSlug},
		})
	})
}
