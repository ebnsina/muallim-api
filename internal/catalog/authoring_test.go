package catalog_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/catalog"
	"github.com/ebnsina/lms-api/internal/platform/database"
)

// seedAuthor creates a real user and membership. audit_log.actor_id is a foreign
// key to users, so an audited action needs an actor who exists — which is the
// point of the constraint.
func seedAuthor(t *testing.T, db *database.DB, tenantID uuid.UUID) catalog.Author {
	t.Helper()

	id := uuid.New()
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO users (id, email, password_hash, name) VALUES ($1, $2, 'x', 'Author')`,
			id, "author-"+id.String()[:8]+"@example.test"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'instructor')`,
			tenantID, id)
		return err
	})
	if err != nil {
		t.Fatalf("seed author: %v", err)
	}
	return catalog.Author{UserID: id}
}

// draftCourse creates a course and returns its slug.
func draftCourse(t *testing.T, svc *catalog.Service, tenantID uuid.UUID, a catalog.Author) string {
	t.Helper()

	slug := "c-" + uuid.NewString()[:8]
	if _, err := svc.CreateCourse(t.Context(), tenantID,
		catalog.NewCourse{Slug: slug, Title: "Course"}, a); err != nil {
		t.Fatalf("CreateCourse: %v", err)
	}
	return slug
}

func TestTopicsAppendInOrderAndCloseGapsOnDelete(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	a := seedAuthor(t, db, tenantID)
	slug := draftCourse(t, svc, tenantID, a)

	var topics []catalog.Topic
	for i := range 4 {
		topic, err := svc.AddTopic(t.Context(), tenantID, slug,
			catalog.NewTopic{Title: fmt.Sprintf("Topic %d", i)}, a)
		if err != nil {
			t.Fatalf("AddTopic: %v", err)
		}
		if topic.Position != i {
			t.Errorf("topic %d appended at position %d, want %d", i, topic.Position, i)
		}
		topics = append(topics, topic)
	}

	// Remove the second. The rest must close up rather than leave a hole.
	if err := svc.RemoveTopic(t.Context(), tenantID, topics[1].ID, a); err != nil {
		t.Fatalf("RemoveTopic: %v", err)
	}

	curriculum, err := svc.Curriculum(t.Context(), tenantID, slug, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(curriculum.Topics) != 3 {
		t.Fatalf("topics = %d, want 3", len(curriculum.Topics))
	}
	for i, topic := range curriculum.Topics {
		if topic.Position != i {
			t.Errorf("after deletion, topic %d has position %d; positions must stay dense", i, topic.Position)
		}
	}
}

// A reorder rewrites every position in one statement. The unique constraint is
// deferred precisely so a full reversal is legal mid-statement.
func TestReorderTopicsReversesInOneStatement(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	a := seedAuthor(t, db, tenantID)
	slug := draftCourse(t, svc, tenantID, a)

	var ids []uuid.UUID
	for i := range 5 {
		topic, err := svc.AddTopic(t.Context(), tenantID, slug, catalog.NewTopic{Title: fmt.Sprint(i)}, a)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, topic.ID)
	}

	reversed := make([]uuid.UUID, len(ids))
	for i, id := range ids {
		reversed[len(ids)-1-i] = id
	}

	if err := svc.ReorderTopics(t.Context(), tenantID, slug, reversed, a); err != nil {
		t.Fatalf("ReorderTopics: %v", err)
	}

	curriculum, err := svc.Curriculum(t.Context(), tenantID, slug, true)
	if err != nil {
		t.Fatal(err)
	}
	for i, topic := range curriculum.Topics {
		if topic.ID != reversed[i] {
			t.Errorf("position %d holds %s, want %s", i, topic.ID, reversed[i])
		}
		if topic.Position != i {
			t.Errorf("topic at index %d has position %d", i, topic.Position)
		}
	}
}

// A partial order would leave the unnamed siblings wherever they were, producing
// an order the author never asked for and cannot see.
func TestReorderRefusesAnIncompleteOrder(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	a := seedAuthor(t, db, tenantID)
	slug := draftCourse(t, svc, tenantID, a)

	var ids []uuid.UUID
	for i := range 3 {
		topic, err := svc.AddTopic(t.Context(), tenantID, slug, catalog.NewTopic{Title: fmt.Sprint(i)}, a)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, topic.ID)
	}

	tests := map[string][]uuid.UUID{
		"too few":    ids[:2],
		"duplicate":  {ids[0], ids[0], ids[1]},
		"foreign id": {ids[0], ids[1], uuid.New()},
		"too many":   {ids[0], ids[1], ids[2], uuid.New()},
	}

	for name, order := range tests {
		t.Run(name, func(t *testing.T) {
			err := svc.ReorderTopics(t.Context(), tenantID, slug, order, a)
			if err == nil {
				t.Fatal("an invalid order was accepted")
			}
			if !errors.Is(err, catalog.ErrIncompleteOrder) && !errors.Is(err, catalog.ErrNotFound) {
				t.Errorf("err = %v, want ErrIncompleteOrder", err)
			}
		})
	}

	// And nothing moved.
	curriculum, err := svc.Curriculum(t.Context(), tenantID, slug, true)
	if err != nil {
		t.Fatal(err)
	}
	for i, topic := range curriculum.Topics {
		if topic.ID != ids[i] {
			t.Errorf("a refused reorder still moved things: position %d holds %s", i, topic.ID)
		}
	}
}

func TestLessonsAppendReorderAndCloseGaps(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	a := seedAuthor(t, db, tenantID)
	slug := draftCourse(t, svc, tenantID, a)

	topic, err := svc.AddTopic(t.Context(), tenantID, slug, catalog.NewTopic{Title: "T"}, a)
	if err != nil {
		t.Fatal(err)
	}

	var ids []uuid.UUID
	for i := range 4 {
		lesson, err := svc.AddLesson(t.Context(), tenantID, topic.ID, catalog.NewLesson{
			Title: fmt.Sprintf("Lesson %d", i), ContentType: "text", VideoSource: catalog.VideoNone,
		}, a)
		if err != nil {
			t.Fatalf("AddLesson: %v", err)
		}
		if lesson.Position != i {
			t.Errorf("lesson appended at %d, want %d", lesson.Position, i)
		}
		ids = append(ids, lesson.ID)
	}

	// Move the last to the front.
	moved := append([]uuid.UUID{ids[3]}, ids[:3]...)
	if err := svc.ReorderLessons(t.Context(), tenantID, topic.ID, moved, a); err != nil {
		t.Fatalf("ReorderLessons: %v", err)
	}

	curriculum, err := svc.Curriculum(t.Context(), tenantID, slug, true)
	if err != nil {
		t.Fatal(err)
	}
	for i, lesson := range curriculum.Topics[0].Lessons {
		if lesson.ID != moved[i] {
			t.Errorf("lesson position %d holds %s, want %s", i, lesson.ID, moved[i])
		}
	}

	if err := svc.RemoveLesson(t.Context(), tenantID, moved[0], a); err != nil {
		t.Fatal(err)
	}
	curriculum, err = svc.Curriculum(t.Context(), tenantID, slug, true)
	if err != nil {
		t.Fatal(err)
	}
	for i, lesson := range curriculum.Topics[0].Lessons {
		if lesson.Position != i {
			t.Errorf("after deletion, lesson %d has position %d", i, lesson.Position)
		}
	}
}

// The bug this phase found: an unpublished course was served to anyone who knew
// its slug, and marked publicly cacheable.
func TestDraftsAreInvisibleWithoutAuthorRights(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	a := seedAuthor(t, db, tenantID)
	slug := draftCourse(t, svc, tenantID, a)

	// A reader who may not see drafts gets the same answer as for a course that
	// does not exist.
	_, err := svc.Curriculum(t.Context(), tenantID, slug, false)
	if !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound — an unpublished course was served to a reader", err)
	}

	// An author sees it.
	if _, err := svc.Curriculum(t.Context(), tenantID, slug, true); err != nil {
		t.Errorf("an author cannot see their own draft: %v", err)
	}

	// Nor does it appear in the public list.
	page, err := svc.ListCourses(t.Context(), tenantID, catalog.ListParams{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range page.Courses {
		if c.Slug == slug {
			t.Error("a draft appeared in the published catalog")
		}
	}

	// It does appear once the caller has been authorised to see drafts.
	authored, err := svc.ListCourses(t.Context(), tenantID,
		catalog.ListParams{Limit: 50, IncludeDrafts: true})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range authored.Courses {
		if c.Slug == slug {
			found = true
		}
	}
	if !found {
		t.Error("an author cannot find their own draft in the authored listing")
	}
}

// The zero value of ListParams lists published courses. A caller who forgets to
// think about drafts must leak none.
func TestListParamsZeroValueHidesDrafts(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	a := seedAuthor(t, db, tenantID)
	slug := draftCourse(t, svc, tenantID, a)

	page, err := svc.ListCourses(t.Context(), tenantID, catalog.ListParams{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range page.Courses {
		if c.Slug == slug {
			t.Fatal("the zero value of ListParams surfaced a draft")
		}
	}
}

func TestPublishRequiresAtLeastOneLesson(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	a := seedAuthor(t, db, tenantID)
	slug := draftCourse(t, svc, tenantID, a)

	if _, err := svc.Publish(t.Context(), tenantID, slug, a); !errors.Is(err, catalog.ErrEmptyCourse) {
		t.Fatalf("err = %v, want ErrEmptyCourse", err)
	}

	// A topic alone is not enough.
	topic, err := svc.AddTopic(t.Context(), tenantID, slug, catalog.NewTopic{Title: "T"}, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Publish(t.Context(), tenantID, slug, a); !errors.Is(err, catalog.ErrEmptyCourse) {
		t.Errorf("a course with an empty topic was published: %v", err)
	}

	if _, err := svc.AddLesson(t.Context(), tenantID, topic.ID, catalog.NewLesson{
		Title: "L", ContentType: "text", VideoSource: catalog.VideoNone,
	}, a); err != nil {
		t.Fatal(err)
	}

	published, err := svc.Publish(t.Context(), tenantID, slug, a)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if published.Status != catalog.StatusPublished {
		t.Errorf("status = %q, want published", published.Status)
	}
	if published.PublishedAt == nil {
		t.Error("published_at was not stamped")
	}

	// Now visible to a reader.
	if _, err := svc.Curriculum(t.Context(), tenantID, slug, false); err != nil {
		t.Errorf("a published course is not visible to a reader: %v", err)
	}

	// Publishing twice is a conflict, not a silent no-op.
	if _, err := svc.Publish(t.Context(), tenantID, slug, a); !errors.Is(err, catalog.ErrAlreadyPublished) {
		t.Errorf("err = %v, want ErrAlreadyPublished", err)
	}
}

// published_at records when a course first went live. Unpublishing does not erase
// that fact, and republishing does not rewrite it.
func TestUnpublishKeepsTheOriginalPublishedAt(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	a := seedAuthor(t, db, tenantID)
	slug := draftCourse(t, svc, tenantID, a)

	topic, err := svc.AddTopic(t.Context(), tenantID, slug, catalog.NewTopic{Title: "T"}, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddLesson(t.Context(), tenantID, topic.ID, catalog.NewLesson{
		Title: "L", ContentType: "text", VideoSource: catalog.VideoNone,
	}, a); err != nil {
		t.Fatal(err)
	}

	first, err := svc.Publish(t.Context(), tenantID, slug, a)
	if err != nil {
		t.Fatal(err)
	}
	original := *first.PublishedAt

	drafted, err := svc.Unpublish(t.Context(), tenantID, slug, a)
	if err != nil {
		t.Fatalf("Unpublish: %v", err)
	}
	if drafted.Status != catalog.StatusDraft {
		t.Errorf("status = %q, want draft", drafted.Status)
	}
	if drafted.PublishedAt == nil || !drafted.PublishedAt.Equal(original) {
		t.Error("unpublishing erased when the course first went live")
	}

	// Hidden again.
	if _, err := svc.Curriculum(t.Context(), tenantID, slug, false); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("an unpublished course is still visible to readers: %v", err)
	}

	again, err := svc.Publish(t.Context(), tenantID, slug, a)
	if err != nil {
		t.Fatal(err)
	}
	if !again.PublishedAt.Equal(original) {
		t.Error("republishing rewrote the original publication date")
	}
}

func TestLessonValidation(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	a := seedAuthor(t, db, tenantID)
	slug := draftCourse(t, svc, tenantID, a)

	topic, err := svc.AddTopic(t.Context(), tenantID, slug, catalog.NewTopic{Title: "T"}, a)
	if err != nil {
		t.Fatal(err)
	}

	tests := map[string]catalog.NewLesson{
		"video lesson with no source": {Title: "L", ContentType: "video", VideoSource: catalog.VideoNone},
		"source with no url":          {Title: "L", ContentType: "video", VideoSource: catalog.VideoYouTube},
		"negative duration":           {Title: "L", ContentType: "text", VideoSource: catalog.VideoNone, DurationSeconds: -1},
	}

	for name, lesson := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.AddLesson(t.Context(), tenantID, topic.ID, lesson, a); !errors.Is(err, catalog.ErrInvalidLesson) {
				t.Errorf("err = %v, want ErrInvalidLesson", err)
			}
		})
	}
}

// Editing must not turn the curriculum read into an N+1, and authoring writes
// must not quietly multiply either.
func TestAuthoringWritesAreBounded(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	a := seedAuthor(t, db, tenantID)
	slug := draftCourse(t, svc, tenantID, a)

	topic, err := svc.AddTopic(t.Context(), tenantID, slug, catalog.NewTopic{Title: "T"}, a)
	if err != nil {
		t.Fatal(err)
	}

	// Reordering N lessons must cost a bounded number of queries, not one per
	// lesson.
	var ids []uuid.UUID
	for i := range 20 {
		lesson, err := svc.AddLesson(t.Context(), tenantID, topic.ID, catalog.NewLesson{
			Title: fmt.Sprint(i), ContentType: "text", VideoSource: catalog.VideoNone,
		}, a)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, lesson.ID)
	}

	counter := &database.Counter{}
	ctx := database.WithCounter(t.Context(), counter)

	if err := svc.ReorderLessons(ctx, tenantID, topic.ID, ids, a); err != nil {
		t.Fatal(err)
	}

	// count siblings, the UPDATE, the audit insert.
	const want = 3
	if got := counter.Count(); got != want {
		t.Errorf("reordering 20 lessons issued %d queries, want %d — a reorder must not be one query per row", got, want)
	}
}
