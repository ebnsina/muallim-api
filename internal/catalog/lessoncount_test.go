package catalog_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/ebnsina/muallim-api/internal/catalog"
	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// seedCourseWithLessons creates a draft course with one topic holding `lessons`
// lessons, and returns its slug.
func seedCourseWithLessons(t *testing.T, svc *catalog.Service, tenantID uuid.UUID, a catalog.Author, lessons int) string {
	t.Helper()

	slug := draftCourse(t, svc, tenantID, a)
	topic, err := svc.AddTopic(t.Context(), tenantID, slug, catalog.NewTopic{Title: "T"}, a)
	if err != nil {
		t.Fatalf("AddTopic: %v", err)
	}
	for i := range lessons {
		if _, err := svc.AddLesson(t.Context(), tenantID, topic.ID, catalog.NewLesson{
			Title: fmt.Sprint(i), ContentType: "text", VideoSource: catalog.VideoNone,
		}, a); err != nil {
			t.Fatalf("AddLesson: %v", err)
		}
	}
	return slug
}

// The listing reports each course's lesson count: a course with lessons, one with
// a topic but no lessons, and a bare course all report the number they hold.
func TestListingCoursesReportsLessonCount(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	a := seedAuthor(t, db, tenantID)

	full := seedCourseWithLessons(t, svc, tenantID, a, 3)
	emptyTopic := seedCourseWithLessons(t, svc, tenantID, a, 0)
	bare := draftCourse(t, svc, tenantID, a)

	page, err := svc.ListCourses(t.Context(), tenantID, catalog.ListParams{Limit: 20, IncludeDrafts: true})
	if err != nil {
		t.Fatalf("ListCourses: %v", err)
	}

	got := make(map[string]int, len(page.Courses))
	for _, c := range page.Courses {
		got[c.Slug] = c.LessonCount
	}

	for slug, want := range map[string]int{full: 3, emptyTopic: 0, bare: 0} {
		if got[slug] != want {
			t.Errorf("course %q reported %d lessons, want %d", slug, got[slug], want)
		}
	}
}

// Counting the lessons must not cost one query per course. A page of many courses
// issues the same number of statements as a page of few — the count is batched.
func TestListingCoursesCountsLessonsWithoutNPlusOne(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	a := seedAuthor(t, db, tenantID)

	measure := func() int {
		counter := &database.Counter{}
		ctx := database.WithCounter(t.Context(), counter)
		if _, err := svc.ListCourses(ctx, tenantID, catalog.ListParams{Limit: 20, IncludeDrafts: true}); err != nil {
			t.Fatalf("ListCourses: %v", err)
		}
		return counter.Count()
	}

	seedCourseWithLessons(t, svc, tenantID, a, 3)
	seedCourseWithLessons(t, svc, tenantID, a, 2)
	few := measure()

	for range 6 {
		seedCourseWithLessons(t, svc, tenantID, a, 4)
	}
	many := measure()

	if few != many {
		t.Errorf("listing 2 courses took %d queries, 8 took %d — the lesson count must be batched, not one query per course", few, many)
	}
}
