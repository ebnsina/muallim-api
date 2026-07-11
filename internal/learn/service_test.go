package learn_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/learn"
	"github.com/ebnsina/lms-api/internal/platform/database"
)

func testDB(t *testing.T) *database.DB {
	t.Helper()

	url := os.Getenv("LMS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LMS_TEST_DATABASE_URL is not set; skipping database tests")
	}

	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	db, err := database.New(t.Context(), database.Options{URL: url, MaxConns: 4}, log)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func seedTenant(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()

	id := uuid.New()
	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, subdomain, name) VALUES ($1, $2, 'Test')`,
			id, "t"+id.String()[:8])
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_ = db.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, id)
			return err
		})
	})
	return id
}

func seedUser(t *testing.T, db *database.DB, tenantID uuid.UUID) uuid.UUID {
	t.Helper()

	id := uuid.New()
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO users (id, email, password_hash, name) VALUES ($1, $2, 'x', 'Learner')`,
			id, "u-"+id.String()[:8]+"@example.test"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'student')`, tenantID, id)
		return err
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// seedLesson creates a course, a topic, and a lesson, returning the lesson id —
// the foreign key a note points at.
func seedLesson(t *testing.T, db *database.DB, tenantID uuid.UUID) uuid.UUID {
	t.Helper()

	var lessonID uuid.UUID
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var courseID, topicID uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO courses (tenant_id, slug, title, status)
			 VALUES ($1, $2, 'Course', 'published') RETURNING id`,
			tenantID, "c-"+uuid.NewString()[:8]).Scan(&courseID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO topics (tenant_id, course_id, title, position) VALUES ($1, $2, 'T', 0) RETURNING id`,
			tenantID, courseID).Scan(&topicID); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`INSERT INTO lessons (tenant_id, topic_id, title, content_type, position)
			 VALUES ($1, $2, 'Lesson', 'text', 0) RETURNING id`,
			tenantID, topicID).Scan(&lessonID)
	})
	if err != nil {
		t.Fatalf("seed lesson: %v", err)
	}
	return lessonID
}

// seedCourseWithLessons creates a course under a topic with two lessons, and
// returns the course slug and both lesson ids — enough for a course-wide read.
func seedCourseWithLessons(t *testing.T, db *database.DB, tenantID uuid.UUID) (string, uuid.UUID, uuid.UUID) {
	t.Helper()

	slug := "c-" + uuid.NewString()[:8]
	var lessonA, lessonB uuid.UUID
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var courseID, topicID uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO courses (tenant_id, slug, title, status)
			 VALUES ($1, $2, 'Course', 'published') RETURNING id`,
			tenantID, slug).Scan(&courseID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO topics (tenant_id, course_id, title, position) VALUES ($1, $2, 'T', 0) RETURNING id`,
			tenantID, courseID).Scan(&topicID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO lessons (tenant_id, topic_id, title, content_type, position)
			 VALUES ($1, $2, 'A', 'text', 0) RETURNING id`, tenantID, topicID).Scan(&lessonA); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`INSERT INTO lessons (tenant_id, topic_id, title, content_type, position)
			 VALUES ($1, $2, 'B', 'text', 1) RETURNING id`, tenantID, topicID).Scan(&lessonB)
	})
	if err != nil {
		t.Fatalf("seed course with lessons: %v", err)
	}
	return slug, lessonA, lessonB
}

func newService(db *database.DB) *learn.Service {
	return learn.NewService(db, learn.NewPostgresRepository(), nil)
}

// A lesson a learner has never noted reads as an empty note, not an error: the
// client renders one shape whether or not there is anything there.
func TestANeverNotedLessonReadsAsEmpty(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	userID := seedUser(t, db, tenantID)
	lessonID := seedLesson(t, db, tenantID)

	note, err := svc.Note(t.Context(), tenantID, userID, lessonID)
	if err != nil {
		t.Fatalf("Note: %v", err)
	}
	if note.Body != "" {
		t.Errorf("a never-noted lesson has body %q, want empty", note.Body)
	}
	if note.LessonID != lessonID {
		t.Errorf("note is for lesson %s, want %s", note.LessonID, lessonID)
	}
}

// Writing a note then reading it back returns what was written; writing again
// replaces it rather than stacking a second one.
func TestSavingANoteAndReplacingIt(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	userID := seedUser(t, db, tenantID)
	lessonID := seedLesson(t, db, tenantID)

	if _, err := svc.Save(t.Context(), tenantID, userID, lessonID, "  first thoughts  "); err != nil {
		t.Fatalf("Save: %v", err)
	}

	note, err := svc.Note(t.Context(), tenantID, userID, lessonID)
	if err != nil {
		t.Fatalf("Note: %v", err)
	}
	// The body is trimmed on the way in.
	if note.Body != "first thoughts" {
		t.Errorf("body = %q, want %q", note.Body, "first thoughts")
	}
	if note.UpdatedAt.IsZero() {
		t.Error("a saved note has no updated_at")
	}

	if _, err := svc.Save(t.Context(), tenantID, userID, lessonID, "on reflection"); err != nil {
		t.Fatalf("Save again: %v", err)
	}
	note, err = svc.Note(t.Context(), tenantID, userID, lessonID)
	if err != nil {
		t.Fatalf("Note again: %v", err)
	}
	if note.Body != "on reflection" {
		t.Errorf("after replacing, body = %q, want %q", note.Body, "on reflection")
	}
}

// Clearing a note — an empty or whitespace body — removes it, and reading returns
// the same empty note as a lesson that was never noted.
func TestAnEmptyBodyClearsTheNote(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	userID := seedUser(t, db, tenantID)
	lessonID := seedLesson(t, db, tenantID)

	if _, err := svc.Save(t.Context(), tenantID, userID, lessonID, "something"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := svc.Save(t.Context(), tenantID, userID, lessonID, "   "); err != nil {
		t.Fatalf("clear: %v", err)
	}

	note, err := svc.Note(t.Context(), tenantID, userID, lessonID)
	if err != nil {
		t.Fatalf("Note: %v", err)
	}
	if note.Body != "" {
		t.Errorf("after clearing, body = %q, want empty", note.Body)
	}
}

// A note is private to the learner who wrote it: another learner on the same
// lesson, in the same workspace, reads their own empty note, not this one.
func TestANoteIsPrivateToItsLearner(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	mine := seedUser(t, db, tenantID)
	other := seedUser(t, db, tenantID)
	lessonID := seedLesson(t, db, tenantID)

	if _, err := svc.Save(t.Context(), tenantID, mine, lessonID, "my private margin"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	note, err := svc.Note(t.Context(), tenantID, other, lessonID)
	if err != nil {
		t.Fatalf("Note: %v", err)
	}
	if note.Body != "" {
		t.Errorf("another learner sees %q; a note is not shared", note.Body)
	}
}

// A note past the limit is refused, not silently truncated: dropping the end of
// what someone wrote is worse than telling them.
func TestANoteTooLongIsRefused(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	userID := seedUser(t, db, tenantID)
	lessonID := seedLesson(t, db, tenantID)

	_, err := svc.Save(t.Context(), tenantID, userID, lessonID, strings.Repeat("a", learn.MaxNoteLength+1))
	if !errors.Is(err, learn.ErrNoteTooLong) {
		t.Errorf("saving an over-long note returned %v, want ErrNoteTooLong", err)
	}
}

// A note against a lesson that does not exist is refused as a not-found, from the
// foreign key rather than a leaked 500.
func TestANoteOnAMissingLessonIsRefused(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	userID := seedUser(t, db, tenantID)

	_, err := svc.Save(t.Context(), tenantID, userID, uuid.New(), "a note on nothing")
	if !errors.Is(err, learn.ErrLessonNotFound) {
		t.Errorf("saving against a missing lesson returned %v, want ErrLessonNotFound", err)
	}
}

// Marks are added, come back in reading order, and each carries its own note.
func TestHighlightsAreListedInReadingOrder(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	userID := seedUser(t, db, tenantID)
	lessonID := seedLesson(t, db, tenantID)

	// Added out of order; listed in the order they sit in the text.
	if _, err := svc.AddHighlight(t.Context(), tenantID, userID, lessonID,
		learn.Highlight{Quote: "second", Note: "later in the text", Start: 20, End: 26}); err != nil {
		t.Fatalf("AddHighlight: %v", err)
	}
	first, err := svc.AddHighlight(t.Context(), tenantID, userID, lessonID,
		learn.Highlight{Quote: "first", Note: "earlier", Start: 3, End: 8})
	if err != nil {
		t.Fatalf("AddHighlight: %v", err)
	}
	if first.ID == uuid.Nil || first.CreatedAt.IsZero() {
		t.Error("a created highlight has no id or timestamp")
	}

	highlights, err := svc.Highlights(t.Context(), tenantID, userID, lessonID)
	if err != nil {
		t.Fatalf("Highlights: %v", err)
	}
	if len(highlights) != 2 {
		t.Fatalf("got %d highlights, want 2", len(highlights))
	}
	if highlights[0].Quote != "first" || highlights[1].Quote != "second" {
		t.Errorf("highlights out of reading order: %q then %q", highlights[0].Quote, highlights[1].Quote)
	}
	if highlights[0].Note != "earlier" {
		t.Errorf("first note = %q, want %q", highlights[0].Note, "earlier")
	}
}

// The note on a mark can be changed, and the mark removed; both refuse a mark that
// is not the caller's, with the same not-found either way.
func TestEditingAndRemovingHighlights(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	mine := seedUser(t, db, tenantID)
	other := seedUser(t, db, tenantID)
	lessonID := seedLesson(t, db, tenantID)

	h, err := svc.AddHighlight(t.Context(), tenantID, mine, lessonID,
		learn.Highlight{Quote: "a passage", Start: 0, End: 9})
	if err != nil {
		t.Fatalf("AddHighlight: %v", err)
	}

	if err := svc.EditHighlightNote(t.Context(), tenantID, mine, h.ID, "  a second thought  "); err != nil {
		t.Fatalf("EditHighlightNote: %v", err)
	}
	got, _ := svc.Highlights(t.Context(), tenantID, mine, lessonID)
	if got[0].Note != "a second thought" {
		t.Errorf("edited note = %q, want %q", got[0].Note, "a second thought")
	}

	// Another learner cannot touch it: not theirs reads as not found.
	if err := svc.EditHighlightNote(t.Context(), tenantID, other, h.ID, "hijack"); !errors.Is(err, learn.ErrHighlightNotFound) {
		t.Errorf("editing another learner's mark returned %v, want ErrHighlightNotFound", err)
	}
	if err := svc.RemoveHighlight(t.Context(), tenantID, other, h.ID); !errors.Is(err, learn.ErrHighlightNotFound) {
		t.Errorf("removing another learner's mark returned %v, want ErrHighlightNotFound", err)
	}

	// The owner removes it, and it is gone.
	if err := svc.RemoveHighlight(t.Context(), tenantID, mine, h.ID); err != nil {
		t.Fatalf("RemoveHighlight: %v", err)
	}
	if got, _ := svc.Highlights(t.Context(), tenantID, mine, lessonID); len(got) != 0 {
		t.Errorf("after removal, %d highlights remain, want 0", len(got))
	}
	// Removing it again is a not-found, not a silent success.
	if err := svc.RemoveHighlight(t.Context(), tenantID, mine, h.ID); !errors.Is(err, learn.ErrHighlightNotFound) {
		t.Errorf("removing a gone mark returned %v, want ErrHighlightNotFound", err)
	}
}

// A mark that could not have come from a selection is refused before the database.
func TestAnInvalidHighlightIsRefused(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	userID := seedUser(t, db, tenantID)
	lessonID := seedLesson(t, db, tenantID)

	cases := map[string]learn.Highlight{
		"empty quote":      {Quote: "", Start: 0, End: 5},
		"end before start": {Quote: "x", Start: 9, End: 3},
		"zero-width":       {Quote: "x", Start: 4, End: 4},
	}
	for name, h := range cases {
		if _, err := svc.AddHighlight(t.Context(), tenantID, userID, lessonID, h); !errors.Is(err, learn.ErrInvalidHighlight) {
			t.Errorf("%s: returned %v, want ErrInvalidHighlight", name, err)
		}
	}
}

// The revision read gathers a learner's notes and marks from across a course's
// lessons, resolved by slug, and only their own.
func TestCourseAnnotationsGathersAcrossLessons(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	mine := seedUser(t, db, tenantID)
	other := seedUser(t, db, tenantID)
	slug, lessonA, lessonB := seedCourseWithLessons(t, db, tenantID)

	if _, err := svc.Save(t.Context(), tenantID, mine, lessonA, "note on A"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := svc.AddHighlight(t.Context(), tenantID, mine, lessonB,
		learn.Highlight{Quote: "a mark on B", Start: 0, End: 11}); err != nil {
		t.Fatalf("AddHighlight: %v", err)
	}
	// Another learner's note on the same course must not leak in.
	if _, err := svc.Save(t.Context(), tenantID, other, lessonA, "someone else's"); err != nil {
		t.Fatalf("Save other: %v", err)
	}

	notes, highlights, err := svc.CourseAnnotations(t.Context(), tenantID, mine, slug)
	if err != nil {
		t.Fatalf("CourseAnnotations: %v", err)
	}
	if len(notes) != 1 || notes[0].Body != "note on A" || notes[0].LessonID != lessonA {
		t.Errorf("notes = %+v, want one on lesson A", notes)
	}
	if len(highlights) != 1 || highlights[0].LessonID != lessonB {
		t.Errorf("highlights = %+v, want one on lesson B", highlights)
	}

	// A slug that is not a course yields nothing rather than erroring.
	notes, highlights, err = svc.CourseAnnotations(t.Context(), tenantID, mine, "no-such-course")
	if err != nil {
		t.Fatalf("CourseAnnotations(unknown): %v", err)
	}
	if len(notes) != 0 || len(highlights) != 0 {
		t.Errorf("an unknown slug yielded %d notes and %d highlights, want none", len(notes), len(highlights))
	}
}
