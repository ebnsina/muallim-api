package catalog_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ebnsina/muallim-api/internal/catalog"
)

func TestTheAuthorIsRecordedAndReadBack(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	a := seedAuthor(t, db, tenantID)
	slug := draftCourse(t, svc, tenantID, a)

	curriculum, err := svc.Curriculum(t.Context(), tenantID, slug, true)
	if err != nil {
		t.Fatalf("Curriculum: %v", err)
	}

	if curriculum.Course.CreatedBy == nil || *curriculum.Course.CreatedBy != a.UserID {
		t.Errorf("created_by = %v, want %v", curriculum.Course.CreatedBy, a.UserID)
	}

	// The name is joined in the course's own query. A page that shows "created by"
	// must not pay a second query for the string.
	if curriculum.Course.InstructorName != "Author" {
		t.Errorf("instructor name = %q, want %q", curriculum.Course.InstructorName, "Author")
	}
}

func TestEditingCourseCopyLeavesUnmentionedFieldsAlone(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	a := seedAuthor(t, db, tenantID)
	slug := draftCourse(t, svc, tenantID, a)

	description := "The long pitch."
	objectives := []string{"Solve a quadratic", "Prove it geometrically"}

	edited, err := svc.EditCourse(t.Context(), tenantID, slug, catalog.CoursePatch{
		Description: &description,
		Objectives:  &objectives,
	}, a)
	if err != nil {
		t.Fatalf("EditCourse: %v", err)
	}

	if edited.Description != description {
		t.Errorf("description = %q, want %q", edited.Description, description)
	}
	if len(edited.Objectives) != 2 {
		t.Errorf("objectives = %v, want 2 entries", edited.Objectives)
	}

	// Title was not mentioned, so it must survive. A patch that silently blanks the
	// fields it was not given is a patch that eats a course every time somebody
	// edits one field of it.
	if edited.Title != "Course" {
		t.Errorf("title = %q, want it untouched at %q", edited.Title, "Course")
	}
	if edited.Language != "en" {
		t.Errorf("language = %q, want the default %q", edited.Language, "en")
	}
}

func TestAnEmptyListClearsTheList(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	a := seedAuthor(t, db, tenantID)
	slug := draftCourse(t, svc, tenantID, a)

	objectives := []string{"Something"}
	if _, err := svc.EditCourse(t.Context(), tenantID, slug,
		catalog.CoursePatch{Objectives: &objectives}, a); err != nil {
		t.Fatalf("EditCourse: %v", err)
	}

	// Sending [] is an act — "this course promises nothing" — and is not the same
	// as omitting the field.
	empty := []string{}
	edited, err := svc.EditCourse(t.Context(), tenantID, slug,
		catalog.CoursePatch{Objectives: &empty}, a)
	if err != nil {
		t.Fatalf("EditCourse: %v", err)
	}
	if len(edited.Objectives) != 0 {
		t.Errorf("objectives = %v, want them cleared", edited.Objectives)
	}
}

func TestCopyThatWouldNotFitOnAPageIsRefused(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	a := seedAuthor(t, db, tenantID)
	slug := draftCourse(t, svc, tenantID, a)

	blank := "   "
	long := strings.Repeat("x", catalog.MaxCourseDescription+1)
	many := make([]string, catalog.MaxCourseListItems+1)
	for i := range many {
		many[i] = "an objective"
	}
	bad := "not-a-difficulty"

	cases := map[string]struct {
		patch catalog.CoursePatch
		want  error
	}{
		"a blank title":            {catalog.CoursePatch{Title: &blank}, catalog.ErrInvalidCourse},
		"a description of a novel": {catalog.CoursePatch{Description: &long}, catalog.ErrInvalidCourse},
		"more objectives than fit": {catalog.CoursePatch{Objectives: &many}, catalog.ErrInvalidCourse},
		"an empty objective":       {catalog.CoursePatch{Objectives: &[]string{"  "}}, catalog.ErrInvalidCourse},
		"a difficulty that is not": {catalog.CoursePatch{Difficulty: &bad}, catalog.ErrInvalidDifficulty},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := svc.EditCourse(t.Context(), tenantID, slug, tc.patch, a)
			if !errors.Is(err, tc.want) {
				t.Errorf("EditCourse(%s) = %v, want %v", name, err, tc.want)
			}
		})
	}
}

func TestEditingACourseThatIsNotThere(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	a := seedAuthor(t, db, tenantID)

	title := "A title"
	_, err := svc.EditCourse(t.Context(), tenantID, "no-such-course",
		catalog.CoursePatch{Title: &title}, a)

	if !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("EditCourse = %v, want ErrNotFound", err)
	}
}
