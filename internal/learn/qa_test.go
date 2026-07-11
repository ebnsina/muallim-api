package learn_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/learn"
	"github.com/ebnsina/lms-api/internal/platform/database"
)

// seedLessonInCourse makes a published course with one lesson and returns the
// course id and lesson id, so a test can enrol a learner against the course.
func seedLessonInCourse(t *testing.T, db *database.DB, tenantID uuid.UUID, preview bool) (courseID, lessonID uuid.UUID) {
	t.Helper()
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var topicID uuid.UUID
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
			`INSERT INTO lessons (tenant_id, topic_id, title, content_type, position, is_preview)
			 VALUES ($1, $2, 'Lesson', 'text', 0, $3) RETURNING id`,
			tenantID, topicID, preview).Scan(&lessonID)
	})
	if err != nil {
		t.Fatalf("seed lesson in course: %v", err)
	}
	return courseID, lessonID
}

func enrolLearner(t *testing.T, db *database.DB, tenantID, courseID, userID uuid.UUID) {
	t.Helper()
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO enrolments (tenant_id, course_id, user_id, source) VALUES ($1, $2, $3, 'self')`,
			tenantID, courseID, userID)
		return err
	})
	if err != nil {
		t.Fatalf("enrol learner: %v", err)
	}
}

func TestQAFollowsLessonAccess(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	courseID, lessonID := seedLessonInCourse(t, db, tenantID, false)

	outsider := seedUser(t, db, tenantID)
	learner := seedUser(t, db, tenantID)

	// A learner not enrolled on a non-preview lesson may not ask — 404, not 403.
	if _, err := svc.Ask(t.Context(), tenantID, lessonID, learn.Participant{UserID: outsider}, "why?"); !errors.Is(err, learn.ErrLessonNotFound) {
		t.Fatalf("outsider ask: got %v, want ErrLessonNotFound", err)
	}

	enrolLearner(t, db, tenantID, courseID, learner)
	q, err := svc.Ask(t.Context(), tenantID, lessonID, learn.Participant{UserID: learner}, "  Why is the sky blue?  ")
	if err != nil {
		t.Fatalf("enrolled ask: %v", err)
	}
	if q.Body != "Why is the sky blue?" {
		t.Fatalf("body not trimmed: %q", q.Body)
	}

	// An instructor (moderator) answers without an enrolment, and is badged.
	instructor := seedUser(t, db, tenantID)
	ans, err := svc.Answer(t.Context(), tenantID, q.ID, learn.Participant{UserID: instructor, CanModerate: true}, "Rayleigh scattering.")
	if err != nil {
		t.Fatalf("instructor answer: %v", err)
	}
	if !ans.ByInstructor {
		t.Fatalf("instructor answer not badged")
	}

	// The thread reads back with its answer, for an enrolled reader.
	threads, err := svc.Questions(t.Context(), tenantID, lessonID, learn.Participant{UserID: learner}, 50)
	if err != nil {
		t.Fatalf("questions: %v", err)
	}
	if len(threads) != 1 || len(threads[0].Answers) != 1 {
		t.Fatalf("thread shape: %d questions, answers %v", len(threads), threads)
	}

	// The outsider reads an empty thread, not the enrolled learner's.
	empty, err := svc.Questions(t.Context(), tenantID, lessonID, learn.Participant{UserID: outsider}, 50)
	if err != nil {
		t.Fatalf("outsider questions: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("outsider saw %d threads, want 0", len(empty))
	}

	// A different learner cannot delete someone else's question; the author can.
	if err := svc.DeleteQuestion(t.Context(), tenantID, q.ID, learn.Participant{UserID: instructor}); !errors.Is(err, learn.ErrQuestionNotFound) {
		// instructor is not a moderator here (no flag) and not the author.
		t.Fatalf("non-owner delete: got %v, want ErrQuestionNotFound", err)
	}
	if err := svc.DeleteQuestion(t.Context(), tenantID, q.ID, learn.Participant{UserID: learner}); err != nil {
		t.Fatalf("author delete: %v", err)
	}
}

func TestQAPreviewLessonIsOpenToAnyone(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	_, lessonID := seedLessonInCourse(t, db, tenantID, true)

	anyone := seedUser(t, db, tenantID)
	if _, err := svc.Ask(t.Context(), tenantID, lessonID, learn.Participant{UserID: anyone}, "First!"); err != nil {
		t.Fatalf("ask on preview: %v", err)
	}
}

func TestAnswerRejectsEmptyBody(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	courseID, lessonID := seedLessonInCourse(t, db, tenantID, false)
	learner := seedUser(t, db, tenantID)
	enrolLearner(t, db, tenantID, courseID, learner)

	q, err := svc.Ask(t.Context(), tenantID, lessonID, learn.Participant{UserID: learner}, "real question")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if _, err := svc.Answer(t.Context(), tenantID, q.ID, learn.Participant{UserID: learner}, "   "); !errors.Is(err, learn.ErrEmptyPost) {
		t.Fatalf("empty answer: got %v, want ErrEmptyPost", err)
	}
}

// The discussion read is two queries — the questions, then all their answers —
// whatever the thread count. Growing the fixture must not grow the query count,
// or a busy lesson becomes one query per question.
func TestQuestionsAreNotNPlusOne(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	courseID, lessonID := seedLessonInCourse(t, db, tenantID, false)
	learner := seedUser(t, db, tenantID)
	enrolLearner(t, db, tenantID, courseID, learner)
	reader := learn.Participant{UserID: learner}

	count := func() int {
		counter := &database.Counter{}
		ctx := database.WithCounter(t.Context(), counter)
		if _, err := svc.Questions(ctx, tenantID, lessonID, reader, 50); err != nil {
			t.Fatalf("questions: %v", err)
		}
		return counter.Count()
	}

	ask := func(n int) {
		for range n {
			q, err := svc.Ask(t.Context(), tenantID, lessonID, reader, "q")
			if err != nil {
				t.Fatalf("ask: %v", err)
			}
			if _, err := svc.Answer(t.Context(), tenantID, q.ID, reader, "a"); err != nil {
				t.Fatalf("answer: %v", err)
			}
		}
	}

	ask(2)
	small := count()
	ask(6)
	large := count()

	if small != large {
		t.Fatalf("query count grew with threads: %d then %d — an N+1", small, large)
	}
	if large != 2 {
		t.Fatalf("discussion read issued %d queries, want 2", large)
	}
}
