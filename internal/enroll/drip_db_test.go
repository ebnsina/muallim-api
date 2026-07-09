package enroll_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/enroll"
	"github.com/ebnsina/lms-api/internal/platform/database"
)

// setDripMode writes the course's mode directly. The catalog package owns the
// authoring path; what this package must prove is that the mode is honoured.
func setDripMode(t *testing.T, db *database.DB, tenantID uuid.UUID, slug, mode string) {
	t.Helper()

	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE courses SET drip_mode = $3 WHERE tenant_id = $1 AND slug = $2`,
			tenantID, slug, mode)
		return err
	})
	if err != nil {
		t.Fatalf("set drip mode: %v", err)
	}
}

func setLessonSchedule(t *testing.T, db *database.DB, tenantID, lessonID uuid.UUID, at *time.Time, afterDays *int) {
	t.Helper()

	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE lessons SET available_at = $3, available_after_days = $4
			 WHERE tenant_id = $1 AND id = $2`,
			tenantID, lessonID, at, afterDays)
		return err
	})
	if err != nil {
		t.Fatalf("set lesson schedule: %v", err)
	}
}

// Sequential drip, against real rows and the real curriculum order.
//
// seedCourse makes the first lesson a preview, which is never dripped, so the
// gate is visible from the second lesson onward.
func TestSequentialDripOpensOneLessonAtATime(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)

	slug, lessons := seedCourse(t, db, tenantID, 3, true)
	setDripMode(t, db, tenantID, slug, enroll.DripSequential)

	if _, err := svc.Enrol(t.Context(), tenantID, slug, actorFor(learner), enroll.SourceSelf); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	reader := enroll.Reader{UserID: learner}

	// The first lesson is a preview: open, and never dripped.
	if _, access, err := svc.Lesson(t.Context(), tenantID, lessons[0], reader); err != nil || access != enroll.AccessEnrolled {
		t.Fatalf("first lesson: access=%v err=%v", access, err)
	}

	// The second is locked until the first is finished.
	_, _, err := svc.Lesson(t.Context(), tenantID, lessons[1], reader)
	if !errors.Is(err, enroll.ErrLessonLocked) {
		t.Fatalf("second lesson before finishing the first = %v, want ErrLessonLocked", err)
	}

	// Locked is not "not enrolled": nobody should be sent to buy what they own.
	if errors.Is(err, enroll.ErrNotEnrolled) {
		t.Error("a locked lesson reported the learner as not enrolled")
	}

	// Sequential opens on an event, so there is no date to offer.
	var locked *enroll.LessonLocked
	if !errors.As(err, &locked) {
		t.Fatalf("error does not carry the unlock detail: %T", err)
	}
	if locked.AvailableAt != nil {
		t.Errorf("AvailableAt = %v, want nil — nobody knows when they will finish", locked.AvailableAt)
	}

	// A locked lesson cannot be completed either.
	if _, err := svc.CompleteLesson(t.Context(), tenantID, lessons[1], actorFor(learner), true); !errors.Is(err, enroll.ErrLessonLocked) {
		t.Errorf("completing a locked lesson = %v, want ErrLessonLocked", err)
	}

	// Finish the first; the second opens, the third does not.
	if _, err := svc.CompleteLesson(t.Context(), tenantID, lessons[0], actorFor(learner), true); err != nil {
		t.Fatalf("complete first: %v", err)
	}
	if _, access, err := svc.Lesson(t.Context(), tenantID, lessons[1], reader); err != nil || access != enroll.AccessEnrolled {
		t.Fatalf("second lesson after finishing the first: access=%v err=%v", access, err)
	}
	if _, _, err := svc.Lesson(t.Context(), tenantID, lessons[2], reader); !errors.Is(err, enroll.ErrLessonLocked) {
		t.Errorf("third lesson = %v, want ErrLessonLocked", err)
	}

	// And reopening the first shuts the second again. The gate reads progress, and
	// progress moved.
	if _, err := svc.CompleteLesson(t.Context(), tenantID, lessons[0], actorFor(learner), false); err != nil {
		t.Fatalf("reopen first: %v", err)
	}
	if _, _, err := svc.Lesson(t.Context(), tenantID, lessons[1], reader); !errors.Is(err, enroll.ErrLessonLocked) {
		t.Errorf("second lesson after reopening the first = %v, want ErrLessonLocked", err)
	}
}

// Scheduled drip: one instant, the same for everybody.
func TestScheduledDrip(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)

	slug, lessons := seedCourse(t, db, tenantID, 2, true)
	setDripMode(t, db, tenantID, slug, enroll.DripScheduled)

	tomorrow := time.Now().Add(24 * time.Hour)
	setLessonSchedule(t, db, tenantID, lessons[1], &tomorrow, nil)

	if _, err := svc.Enrol(t.Context(), tenantID, slug, actorFor(learner), enroll.SourceSelf); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	reader := enroll.Reader{UserID: learner}

	_, _, err := svc.Lesson(t.Context(), tenantID, lessons[1], reader)

	var locked *enroll.LessonLocked
	if !errors.As(err, &locked) {
		t.Fatalf("lesson = %v, want LessonLocked", err)
	}
	if locked.AvailableAt == nil || !locked.AvailableAt.Equal(tomorrow.UTC()) {
		t.Errorf("AvailableAt = %v, want %v", locked.AvailableAt, tomorrow.UTC())
	}

	// Move the date into the past: it opens, and the reader is told when it did.
	yesterday := time.Now().Add(-24 * time.Hour)
	setLessonSchedule(t, db, tenantID, lessons[1], &yesterday, nil)

	lesson, access, err := svc.Lesson(t.Context(), tenantID, lessons[1], reader)
	if err != nil || access != enroll.AccessEnrolled {
		t.Fatalf("after the date passed: access=%v err=%v", access, err)
	}
	if lesson.Content == "" {
		t.Error("an open lesson returned no body")
	}
}

// After-enrolment drip counts from each learner's own enrolment, so two people
// who enrolled on different days see different dates.
func TestAfterEnrolmentDripIsPerLearner(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	newcomer := seedUser(t, db, tenantID)
	veteran := seedUser(t, db, tenantID)

	slug, lessons := seedCourse(t, db, tenantID, 2, true)
	setDripMode(t, db, tenantID, slug, enroll.DripAfterEnrolment)

	three := 3
	setLessonSchedule(t, db, tenantID, lessons[1], nil, &three)

	for _, learner := range []uuid.UUID{newcomer, veteran} {
		if _, err := svc.Enrol(t.Context(), tenantID, slug, actorFor(learner), enroll.SourceSelf); err != nil {
			t.Fatalf("enrol: %v", err)
		}
	}

	// Backdate the veteran's enrolment by a week.
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE enrolments e SET enrolled_at = now() - interval '7 days'
			FROM courses c
			WHERE c.id = e.course_id AND c.tenant_id = e.tenant_id
			  AND e.tenant_id = $1 AND c.slug = $2 AND e.user_id = $3`,
			tenantID, slug, veteran)
		return err
	})
	if err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// The newcomer waits three days.
	_, _, err = svc.Lesson(t.Context(), tenantID, lessons[1], enroll.Reader{UserID: newcomer})
	if !errors.Is(err, enroll.ErrLessonLocked) {
		t.Errorf("newcomer = %v, want ErrLessonLocked", err)
	}

	// The veteran, who enrolled a week ago, does not.
	if _, access, err := svc.Lesson(t.Context(), tenantID, lessons[1], enroll.Reader{UserID: veteran}); err != nil || access != enroll.AccessEnrolled {
		t.Errorf("veteran: access=%v err=%v, want enrolled", access, err)
	}
}

// An author reads their own dripped lesson. They wrote it; waiting for it would
// be absurd.
func TestDripDoesNotLockAnAuthor(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)

	slug, lessons := seedCourse(t, db, tenantID, 2, true)
	setDripMode(t, db, tenantID, slug, enroll.DripSequential)

	author := enroll.Reader{UserID: seedUser(t, db, tenantID), CanAuthor: true}
	if _, access, err := svc.Lesson(t.Context(), tenantID, lessons[1], author); err != nil || access != enroll.AccessAuthor {
		t.Errorf("author: access=%v err=%v", access, err)
	}
}

// Drip must not cost a round trip. The gate is part of the one query that already
// decides access, and a sequential course must not become the slow one.
func TestDrippedLessonReadIsStillOneQuery(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)

	slug, lessons := seedCourse(t, db, tenantID, 6, true)
	setDripMode(t, db, tenantID, slug, enroll.DripSequential)

	if _, err := svc.Enrol(t.Context(), tenantID, slug, actorFor(learner), enroll.SourceSelf); err != nil {
		t.Fatal(err)
	}

	counter := &database.Counter{}
	ctx := database.WithCounter(t.Context(), counter)

	// The last lesson, so the subquery has five predecessors to count.
	_, _, err := svc.Lesson(ctx, tenantID, lessons[5], enroll.Reader{UserID: learner})
	if !errors.Is(err, enroll.ErrLessonLocked) {
		t.Fatalf("last lesson = %v, want ErrLessonLocked", err)
	}

	if got := counter.Count(); got != 1 {
		t.Errorf("a dripped lesson read issued %d queries, want 1", got)
	}
}
