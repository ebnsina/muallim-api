package catalog_test

import (
	"errors"
	"testing"

	"github.com/ebnsina/lms-api/internal/catalog"
)

// A new course releases everything at once. Drip is opt-in: a course that starts
// dripping because somebody added a column is a course that hides its content.
func TestANewCourseDoesNotDrip(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	author := seedAuthor(t, db, tenantID)

	slug := draftCourse(t, svc, tenantID, author)

	course, _, err := svc.Prerequisites(t.Context(), tenantID, slug, true)
	if err != nil {
		t.Fatalf("load course: %v", err)
	}
	if course.DripMode != catalog.DripNone {
		t.Errorf("drip_mode = %q, want %q", course.DripMode, catalog.DripNone)
	}
}

func TestSetDripMode(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	author := seedAuthor(t, db, tenantID)

	slug := draftCourse(t, svc, tenantID, author)

	for _, mode := range []string{
		catalog.DripScheduled, catalog.DripAfterEnrolment,
		catalog.DripSequential, catalog.DripNone,
	} {
		course, err := svc.SetDripMode(t.Context(), tenantID, slug, mode, author)
		if err != nil {
			t.Fatalf("set %q: %v", mode, err)
		}
		if course.DripMode != mode {
			t.Errorf("drip_mode = %q, want %q", course.DripMode, mode)
		}
	}
}

// An unknown mode is refused rather than treated as "none". A course that
// silently stops dripping publishes its content early, and nobody notices until
// a learner has read it.
func TestUnknownDripModeIsRefused(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	author := seedAuthor(t, db, tenantID)

	slug := draftCourse(t, svc, tenantID, author)

	for _, mode := range []string{"", "weekly", "NONE", "Sequential"} {
		if _, err := svc.SetDripMode(t.Context(), tenantID, slug, mode, author); !errors.Is(err, catalog.ErrInvalidDripMode) {
			t.Errorf("SetDripMode(%q) = %v, want ErrInvalidDripMode", mode, err)
		}
	}

	// And the course is untouched.
	course, _, err := svc.Prerequisites(t.Context(), tenantID, slug, true)
	if err != nil {
		t.Fatalf("load course: %v", err)
	}
	if course.DripMode != catalog.DripNone {
		t.Errorf("a refused mode was written: %q", course.DripMode)
	}
}

func TestSetDripModeOnAnUnknownCourse(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	author := seedAuthor(t, db, tenantID)

	if _, err := svc.SetDripMode(t.Context(), tenantID, "no-such-course", catalog.DripSequential, author); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("SetDripMode on a missing course = %v, want ErrNotFound", err)
	}
}
