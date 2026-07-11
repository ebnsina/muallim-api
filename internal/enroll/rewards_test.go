package enroll_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/audit"
	"github.com/ebnsina/lms-api/internal/enroll"
	"github.com/ebnsina/lms-api/internal/platform/database"
)

// captureRewards records the completions enroll reports, so a test can assert the
// producer fires at the right moments.
type captureRewards struct {
	lessons []uuid.UUID
	courses []uuid.UUID
}

func (r *captureRewards) LessonCompleted(ctx context.Context, tx pgx.Tx, tenantID, userID, lessonID uuid.UUID) error {
	r.lessons = append(r.lessons, lessonID)
	return nil
}

func (r *captureRewards) CourseCompleted(ctx context.Context, tx pgx.Tx, tenantID, userID, courseID uuid.UUID) error {
	r.courses = append(r.courses, courseID)
	return nil
}

func newServiceWithRewards(db *database.DB, rewards enroll.Rewards) *enroll.Service {
	return enroll.NewService(db, enroll.NewPostgresRepository(), enrolAuditor{audit.NewRecorder()}, newIssuer(db), rewards)
}

// Completing lessons awards each once, and finishing the last completes the course
// — one course award, not one per lesson. Reopening awards nothing.
func TestCompletionAwardsLessonsOnceAndTheCourse(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	rewards := &captureRewards{}
	svc := newServiceWithRewards(db, rewards)
	tenantID := seedTenant(t, db)
	slug, lessons := seedCourse(t, db, tenantID, 2, true)
	learner := seedUser(t, db, tenantID)

	if _, err := svc.Enrol(t.Context(), tenantID, slug, actorFor(learner), enroll.SourceSelf); err != nil {
		t.Fatalf("enrol: %v", err)
	}

	// Complete the first lesson twice — the second is a re-completion.
	for range 2 {
		if _, err := svc.CompleteLesson(t.Context(), tenantID, lessons[0], actorFor(learner), true); err != nil {
			t.Fatalf("complete: %v", err)
		}
	}
	// Finish the course by completing the last lesson.
	if _, err := svc.CompleteLesson(t.Context(), tenantID, lessons[1], actorFor(learner), true); err != nil {
		t.Fatalf("complete last: %v", err)
	}

	// enroll reports a lesson completion every time complete=true is called (the
	// award itself dedupes) — but the course exactly once.
	if len(rewards.lessons) != 3 {
		t.Fatalf("lesson completions reported = %d, want 3", len(rewards.lessons))
	}
	if len(rewards.courses) != 1 || rewards.courses[0] == uuid.Nil {
		t.Fatalf("course completions = %v, want exactly one", rewards.courses)
	}

	// Reopening a lesson reports no new completion.
	before := len(rewards.lessons)
	if _, err := svc.CompleteLesson(t.Context(), tenantID, lessons[1], actorFor(learner), false); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if len(rewards.lessons) != before {
		t.Fatalf("reopening a lesson reported a completion")
	}
}
