package catalog_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ebnsina/muallim-api/internal/catalog"
)

func slugsOf(courses []catalog.Course) []string {
	out := make([]string, 0, len(courses))
	for _, c := range courses {
		out = append(out, c.Slug)
	}
	return out
}

// The graph an author builds, read back as they left it.
func TestAddAndRemovePrerequisites(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	author := seedAuthor(t, db, tenantID)

	advanced := draftCourse(t, svc, tenantID, author)
	basics := draftCourse(t, svc, tenantID, author)

	if err := svc.AddPrerequisite(t.Context(), tenantID, advanced, basics, author); err != nil {
		t.Fatalf("add: %v", err)
	}

	_, prereqs, err := svc.Prerequisites(t.Context(), tenantID, advanced, true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := slugsOf(prereqs); len(got) != 1 || got[0] != basics {
		t.Fatalf("prerequisites = %v, want [%s]", got, basics)
	}

	// Adding the same edge twice is a conflict, not a silent success: an author who
	// clicks twice has learned nothing, but one who typed the wrong slug has.
	if err := svc.AddPrerequisite(t.Context(), tenantID, advanced, basics, author); !errors.Is(err, catalog.ErrPrerequisiteExists) {
		t.Errorf("adding a duplicate = %v, want ErrPrerequisiteExists", err)
	}

	if err := svc.RemovePrerequisite(t.Context(), tenantID, advanced, basics, author); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, prereqs, _ = svc.Prerequisites(t.Context(), tenantID, advanced, true); len(prereqs) != 0 {
		t.Errorf("prerequisites survived removal: %v", slugsOf(prereqs))
	}

	// Removing an edge that is not there is a 404, not success.
	if err := svc.RemovePrerequisite(t.Context(), tenantID, advanced, basics, author); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("removing an absent edge = %v, want ErrNotFound", err)
	}
}

// A course cannot require itself. The row constraint would catch it, but the
// service refuses first, with an error that says what happened.
func TestACourseCannotRequireItself(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	author := seedAuthor(t, db, tenantID)

	slug := draftCourse(t, svc, tenantID, author)

	if err := svc.AddPrerequisite(t.Context(), tenantID, slug, slug, author); !errors.Is(err, catalog.ErrPrerequisiteCycle) {
		t.Errorf("self-requirement = %v, want ErrPrerequisiteCycle", err)
	}
}

// The longer loops. A cycle is not a hard course to finish; it is a course
// nobody can ever start, and no message at enrolment time could explain it.
func TestPrerequisiteCyclesAreRefused(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	author := seedAuthor(t, db, tenantID)

	a := draftCourse(t, svc, tenantID, author)
	b := draftCourse(t, svc, tenantID, author)
	c := draftCourse(t, svc, tenantID, author)

	// a requires b requires c.
	if err := svc.AddPrerequisite(t.Context(), tenantID, a, b, author); err != nil {
		t.Fatalf("a requires b: %v", err)
	}
	if err := svc.AddPrerequisite(t.Context(), tenantID, b, c, author); err != nil {
		t.Fatalf("b requires c: %v", err)
	}

	// c requiring a would close the loop, two edges away.
	if err := svc.AddPrerequisite(t.Context(), tenantID, c, a, author); !errors.Is(err, catalog.ErrPrerequisiteCycle) {
		t.Errorf("closing a three-course loop = %v, want ErrPrerequisiteCycle", err)
	}

	// The direct back-edge is the same thing one step shorter.
	if err := svc.AddPrerequisite(t.Context(), tenantID, b, a, author); !errors.Is(err, catalog.ErrPrerequisiteCycle) {
		t.Errorf("closing a two-course loop = %v, want ErrPrerequisiteCycle", err)
	}

	// A diamond is not a cycle. Both a and b may require c, and d may require both.
	d := draftCourse(t, svc, tenantID, author)
	if err := svc.AddPrerequisite(t.Context(), tenantID, d, a, author); err != nil {
		t.Errorf("a diamond was mistaken for a cycle: %v", err)
	}
	if err := svc.AddPrerequisite(t.Context(), tenantID, d, b, author); err != nil {
		t.Errorf("a diamond was mistaken for a cycle: %v", err)
	}
}

// Prerequisites are named by slug, and a slug nobody can see does not exist.
func TestPrerequisitesOfAnUnknownCourseAreNotFound(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	author := seedAuthor(t, db, tenantID)

	slug := draftCourse(t, svc, tenantID, author)
	missing := "no-such-course-" + uuid.NewString()[:8]

	if err := svc.AddPrerequisite(t.Context(), tenantID, slug, missing, author); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("requiring a course that does not exist = %v, want ErrNotFound", err)
	}
	if err := svc.AddPrerequisite(t.Context(), tenantID, missing, slug, author); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("adding to a course that does not exist = %v, want ErrNotFound", err)
	}

	// And a reader who may not see drafts is told the course does not exist,
	// rather than that it exists and is not theirs.
	if _, _, err := svc.Prerequisites(t.Context(), tenantID, slug, false); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("draft prerequisites visible to a stranger: %v", err)
	}
}
