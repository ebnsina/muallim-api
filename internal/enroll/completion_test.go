package enroll_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/enroll"
	"github.com/ebnsina/lms-api/internal/platform/database"
)

// enrolmentRow reads the two fields that state the same fact, so a test can
// catch them disagreeing.
func enrolmentRow(t *testing.T, db *database.DB, tenantID uuid.UUID, slug string, userID uuid.UUID) (status string, completed bool) {
	t.Helper()

	err := db.WithTenantReadOnly(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT e.status, e.completed_at IS NOT NULL
			FROM enrolments e JOIN courses c ON c.id = e.course_id AND c.tenant_id = e.tenant_id
			WHERE e.tenant_id = $1 AND c.slug = $2 AND e.user_id = $3`,
			tenantID, slug, userID).Scan(&status, &completed)
	})
	if err != nil {
		t.Fatalf("read enrolment: %v", err)
	}
	return status, completed
}

// The enrolment's status is a roll-up of the lesson rows, so it moves in both
// directions. A course that stops being finished stops being completed.
func TestReopeningALessonReopensTheEnrolment(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)

	slug, lessons := seedCourse(t, db, tenantID, 2, true)
	finish(t, svc, tenantID, slug, lessons, learner)

	if status, completed := enrolmentRow(t, db, tenantID, slug, learner); status != enroll.StatusCompleted || !completed {
		t.Fatalf("after finishing: status=%q completed_at set=%v", status, completed)
	}

	// Reopen one lesson.
	if _, err := svc.CompleteLesson(t.Context(), tenantID, lessons[0], actorFor(learner), false); err != nil {
		t.Fatalf("reopen lesson: %v", err)
	}

	status, completed := enrolmentRow(t, db, tenantID, slug, learner)
	if status != enroll.StatusActive {
		t.Errorf("status = %q, want active — the course is no longer finished", status)
	}
	if completed {
		t.Error("completed_at survived the reopening; status and timestamp now disagree")
	}

	// And finishing it again completes the enrolment again.
	if _, err := svc.CompleteLesson(t.Context(), tenantID, lessons[0], actorFor(learner), true); err != nil {
		t.Fatalf("re-complete: %v", err)
	}
	if status, completed := enrolmentRow(t, db, tenantID, slug, learner); status != enroll.StatusCompleted || !completed {
		t.Errorf("after re-finishing: status=%q completed_at set=%v", status, completed)
	}
}

// The bug prerequisites found. `CompleteEnrolment` used to guard on
// `completed_at IS NULL`, so an enrolment that had ever been completed could
// never be completed again — and a learner who cancelled, came back, and finished
// the course a second time stayed forever "active" at 100%.
func TestFinishingAgainAfterCancellingCompletesTheEnrolment(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)

	slug, lessons := seedCourse(t, db, tenantID, 1, true)
	finish(t, svc, tenantID, slug, lessons, learner)

	if err := svc.Cancel(t.Context(), tenantID, slug, actorFor(learner)); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// Reactivating must not carry the old completion timestamp back with it.
	if _, err := svc.Enrol(t.Context(), tenantID, slug, actorFor(learner), enroll.SourceSelf); err != nil {
		t.Fatalf("re-enrol: %v", err)
	}
	if status, completed := enrolmentRow(t, db, tenantID, slug, learner); status != enroll.StatusActive || completed {
		t.Fatalf("after re-enrolling: status=%q completed_at set=%v, want active with no timestamp", status, completed)
	}

	// Their progress survived, so the course is already complete: finishing the
	// lesson again is what re-completes the enrolment.
	if _, err := svc.CompleteLesson(t.Context(), tenantID, lessons[0], actorFor(learner), true); err != nil {
		t.Fatalf("re-complete: %v", err)
	}

	if status, completed := enrolmentRow(t, db, tenantID, slug, learner); status != enroll.StatusCompleted || !completed {
		t.Errorf("after finishing a second time: status=%q completed_at set=%v", status, completed)
	}
}

// And the consequence for prerequisites: a course finished, cancelled, and
// finished again still opens the gate.
func TestAPrerequisiteFinishedTwiceStillCounts(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)

	basics, basicsLessons := seedCourse(t, db, tenantID, 1, true)
	advanced, _ := seedCourse(t, db, tenantID, 1, true)
	requirePrerequisite(t, db, tenantID, advanced, basics)

	finish(t, svc, tenantID, basics, basicsLessons, learner)
	if err := svc.Cancel(t.Context(), tenantID, basics, actorFor(learner)); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := svc.Enrol(t.Context(), tenantID, basics, actorFor(learner), enroll.SourceSelf); err != nil {
		t.Fatalf("re-enrol: %v", err)
	}
	if _, err := svc.CompleteLesson(t.Context(), tenantID, basicsLessons[0], actorFor(learner), true); err != nil {
		t.Fatalf("re-complete: %v", err)
	}

	if _, err := svc.Enrol(t.Context(), tenantID, advanced, actorFor(learner), enroll.SourceSelf); err != nil {
		t.Errorf("a prerequisite finished twice did not open the gate: %v", err)
	}
}

// certificateFor reads the certificate a course issued to a learner, if any.
func certificateFor(t *testing.T, db *database.DB, tenantID uuid.UUID, slug string, userID uuid.UUID) (serial string, found bool) {
	t.Helper()

	err := db.WithTenantReadOnly(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT ce.serial
			FROM certificates ce JOIN courses c ON c.id = ce.course_id AND c.tenant_id = ce.tenant_id
			WHERE ce.tenant_id = $1 AND c.slug = $2 AND ce.user_id = $3`,
			tenantID, slug, userID).Scan(&serial)

		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		t.Fatalf("read certificate: %v", err)
	}
	return serial, found
}

/*
Finishing a course issues a certificate, in the transaction that finished it.

The seam is an interface `enroll` declares and `cmd` satisfies over the certify
service. Neither package imports the other, so nothing in either would notice if
the wiring were dropped — except this.
*/
func TestFinishingACourseIssuesACertificate(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)

	slug, lessons := seedCourse(t, db, tenantID, 2, true)

	if _, err := svc.Enrol(t.Context(), tenantID, slug, actorFor(learner), enroll.SourceSelf); err != nil {
		t.Fatalf("enrol: %v", err)
	}

	// Half-way through, there is nothing to show for it.
	if _, err := svc.CompleteLesson(t.Context(), tenantID, lessons[0], actorFor(learner), true); err != nil {
		t.Fatalf("complete first lesson: %v", err)
	}
	if serial, found := certificateFor(t, db, tenantID, slug, learner); found {
		t.Fatalf("a certificate (%s) was issued for half a course", serial)
	}

	finish(t, svc, tenantID, slug, lessons, learner)

	serial, found := certificateFor(t, db, tenantID, slug, learner)
	if !found {
		t.Fatal("the course was finished and no certificate was issued")
	}
	if !strings.HasPrefix(serial, "CERT-") {
		t.Errorf("the serial is %q", serial)
	}
}

/*
Finishing a course twice is finishing it once.

A learner reopens the last lesson and completes it again. The certificate they had
is the certificate they keep — a second one would give them two numbers for one
achievement, and only one of them would be the number they had written down.
*/
func TestReopeningAndRefinishingDoesNotReissue(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)

	slug, lessons := seedCourse(t, db, tenantID, 2, true)
	finish(t, svc, tenantID, slug, lessons, learner)

	first, found := certificateFor(t, db, tenantID, slug, learner)
	if !found {
		t.Fatal("no certificate after finishing")
	}

	// Reopen the last lesson, and finish it again.
	if _, err := svc.CompleteLesson(t.Context(), tenantID, lessons[1], actorFor(learner), false); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	// Reopening does not tear up the certificate. It records that the course was
	// completed on a day, and that day happened.
	if again, found := certificateFor(t, db, tenantID, slug, learner); !found || again != first {
		t.Errorf("reopening a lesson changed the certificate: %q -> %q (found=%v)", first, again, found)
	}

	if _, err := svc.CompleteLesson(t.Context(), tenantID, lessons[1], actorFor(learner), true); err != nil {
		t.Fatalf("refinish: %v", err)
	}

	second, found := certificateFor(t, db, tenantID, slug, learner)
	if !found {
		t.Fatal("the certificate vanished on re-finishing")
	}
	if second != first {
		t.Errorf("re-finishing issued a new certificate: %q, was %q", second, first)
	}
}
