package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrInvalidDripMode is returned for a mode this system does not know.
//
// Refused rather than treated as "none": a course that silently stops dripping is
// a course that publishes its content early, and nobody notices until a learner
// has read it.
var ErrInvalidDripMode = errors.New("catalog: unknown drip mode")

// ActionDripModeChanged records a change to how a course releases its lessons.
const ActionDripModeChanged = "course.drip_mode_changed"

// Drip modes. Restated rather than imported from enroll: a domain package may not
// depend on a sibling, and these strings are part of the schema — the CHECK
// constraint on courses.drip_mode is what actually enforces them.
const (
	DripNone           = "none"
	DripScheduled      = "scheduled"
	DripAfterEnrolment = "after_enrolment"
	DripSequential     = "sequential"
)

// ValidDripMode reports whether mode is one this system knows.
func ValidDripMode(mode string) bool {
	switch mode {
	case DripNone, DripScheduled, DripAfterEnrolment, DripSequential:
		return true
	default:
		return false
	}
}

// SetDripMode changes how a course releases its lessons.
//
// The per-lesson dates are left alone. Switching to a mode that does not read
// them makes them inert rather than deleting them, so an author who switches
// away and back finds their schedule where they left it. Nothing reads a column
// its mode does not name.
func (s *Service) SetDripMode(ctx context.Context, tenantID uuid.UUID, slug, mode string, a Author) (Course, error) {
	if !ValidDripMode(mode) {
		return Course{}, fmt.Errorf("%w: %q", ErrInvalidDripMode, mode)
	}

	var course Course
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		existing, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug, true)
		if err != nil {
			return err
		}

		course, err = s.authoring.SetDripMode(ctx, tx, tenantID, existing.ID, mode)
		if err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &a.UserID, Action: ActionDripModeChanged,
			TargetType: "course", TargetID: existing.ID.String(),
			IP: a.IP, UserAgent: a.UserAgent,
			Metadata: map[string]any{"from": existing.DripMode, "to": mode},
		})
	})
	if err != nil {
		return Course{}, err
	}
	return course, nil
}
