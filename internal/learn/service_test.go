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

func newService(db *database.DB) *learn.Service {
	return learn.NewService(db, learn.NewPostgresRepository())
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
