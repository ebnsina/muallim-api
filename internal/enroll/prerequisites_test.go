package enroll_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/enroll"
	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// requirePrerequisite writes the edge directly. The catalog package owns the
// authoring path and tests it there; what this package must prove is that the
// edge is honoured.
func requirePrerequisite(t *testing.T, db *database.DB, tenantID uuid.UUID, courseSlug, requiresSlug string) {
	t.Helper()

	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO course_prerequisites (tenant_id, course_id, requires_course_id)
			SELECT $1, c.id, r.id
			FROM courses c, courses r
			WHERE c.tenant_id = $1 AND c.slug = $2
			  AND r.tenant_id = $1 AND r.slug = $3`,
			tenantID, courseSlug, requiresSlug)
		return err
	})
	if err != nil {
		t.Fatalf("require prerequisite: %v", err)
	}
}

// finish enrols a learner and completes every lesson, which is what marks the
// enrolment itself completed.
func finish(t *testing.T, svc *enroll.Service, tenantID uuid.UUID, slug string, lessons []uuid.UUID, learner uuid.UUID) {
	t.Helper()

	if _, err := svc.Enrol(t.Context(), tenantID, slug, actorFor(learner), enroll.SourceSelf); err != nil {
		t.Fatalf("enrol on %s: %v", slug, err)
	}
	for _, id := range lessons {
		if _, err := svc.CompleteLesson(t.Context(), tenantID, id, actorFor(learner), true); err != nil {
			t.Fatalf("complete lesson: %v", err)
		}
	}
}

// The gate. A learner who has not finished the prerequisite may not enrol, and is
// told which course stands in the way rather than "some course".
func TestEnrolRefusedUntilPrerequisiteIsComplete(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)

	basics, basicsLessons := seedCourse(t, db, tenantID, 2, true)
	advanced, _ := seedCourse(t, db, tenantID, 2, true)
	requirePrerequisite(t, db, tenantID, advanced, basics)

	_, err := svc.Enrol(t.Context(), tenantID, advanced, actorFor(learner), enroll.SourceSelf)
	if !errors.Is(err, enroll.ErrPrerequisitesUnmet) {
		t.Fatalf("enrol = %v, want ErrPrerequisitesUnmet", err)
	}

	// The caller can name the course without parsing a message.
	var unmet *enroll.UnmetPrerequisites
	if !errors.As(err, &unmet) {
		t.Fatalf("error does not carry the missing courses: %T", err)
	}
	if len(unmet.Missing) != 1 || unmet.Missing[0].Slug != basics {
		t.Fatalf("missing = %+v, want the %s course", unmet.Missing, basics)
	}

	// Enrolling on the prerequisite is not enough. Finishing it is.
	if _, err := svc.Enrol(t.Context(), tenantID, basics, actorFor(learner), enroll.SourceSelf); err != nil {
		t.Fatalf("enrol on the prerequisite: %v", err)
	}
	if _, err := svc.Enrol(t.Context(), tenantID, advanced, actorFor(learner), enroll.SourceSelf); !errors.Is(err, enroll.ErrPrerequisitesUnmet) {
		t.Errorf("enrolling on the prerequisite was treated as finishing it: %v", err)
	}

	for _, id := range basicsLessons {
		if _, err := svc.CompleteLesson(t.Context(), tenantID, id, actorFor(learner), true); err != nil {
			t.Fatalf("complete: %v", err)
		}
	}
	if _, err := svc.Enrol(t.Context(), tenantID, advanced, actorFor(learner), enroll.SourceSelf); err != nil {
		t.Errorf("enrol after finishing the prerequisite: %v", err)
	}
}

// Every prerequisite, not merely one of them.
func TestEnrolNamesEveryUnmetPrerequisite(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)

	first, firstLessons := seedCourse(t, db, tenantID, 1, true)
	second, _ := seedCourse(t, db, tenantID, 1, true)
	advanced, _ := seedCourse(t, db, tenantID, 1, true)
	requirePrerequisite(t, db, tenantID, advanced, first)
	requirePrerequisite(t, db, tenantID, advanced, second)

	_, err := svc.Enrol(t.Context(), tenantID, advanced, actorFor(learner), enroll.SourceSelf)

	var unmet *enroll.UnmetPrerequisites
	if !errors.As(err, &unmet) {
		t.Fatalf("enrol = %v, want UnmetPrerequisites", err)
	}
	if len(unmet.Missing) != 2 {
		t.Fatalf("missing = %d courses, want 2", len(unmet.Missing))
	}

	// Finishing one leaves the other.
	finish(t, svc, tenantID, first, firstLessons, learner)

	_, err = svc.Enrol(t.Context(), tenantID, advanced, actorFor(learner), enroll.SourceSelf)
	if !errors.As(err, &unmet) {
		t.Fatalf("enrol = %v, want UnmetPrerequisites", err)
	}
	if len(unmet.Missing) != 1 || unmet.Missing[0].Slug != second {
		t.Errorf("missing = %+v, want only %s", unmet.Missing, second)
	}
}

// A grant is a deliberate override. An administrator placing a learner on a
// course has already decided the learner belongs there, and refusing them would
// mean nobody could ever be enrolled out of order.
func TestGrantIgnoresPrerequisites(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)
	admin := seedUser(t, db, tenantID)

	basics, _ := seedCourse(t, db, tenantID, 1, true)
	advanced, _ := seedCourse(t, db, tenantID, 1, true)
	requirePrerequisite(t, db, tenantID, advanced, basics)

	if _, err := svc.Grant(t.Context(), tenantID, advanced, learner, actorFor(admin)); err != nil {
		t.Errorf("grant refused by a prerequisite it should override: %v", err)
	}
}

// A prerequisite added after the fact must not lock a learner out of a course
// they are already studying. Their next click is not an enrolment.
func TestAnExistingEnrolmentSurvivesANewPrerequisite(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)

	basics, _ := seedCourse(t, db, tenantID, 1, true)
	advanced, _ := seedCourse(t, db, tenantID, 1, true)

	if _, err := svc.Enrol(t.Context(), tenantID, advanced, actorFor(learner), enroll.SourceSelf); err != nil {
		t.Fatalf("enrol: %v", err)
	}

	requirePrerequisite(t, db, tenantID, advanced, basics)

	if _, err := svc.Enrol(t.Context(), tenantID, advanced, actorFor(learner), enroll.SourceSelf); err != nil {
		t.Errorf("a prerequisite added afterwards locked out an enrolled learner: %v", err)
	}
}

// A cancelled learner is not an enrolled one. Coming back means meeting the gate
// that stands there now.
func TestReEnrollingAfterCancellingMeetsThePrerequisite(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)

	basics, _ := seedCourse(t, db, tenantID, 1, true)
	advanced, _ := seedCourse(t, db, tenantID, 1, true)

	if _, err := svc.Enrol(t.Context(), tenantID, advanced, actorFor(learner), enroll.SourceSelf); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	if err := svc.Cancel(t.Context(), tenantID, advanced, actorFor(learner)); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	requirePrerequisite(t, db, tenantID, advanced, basics)

	if _, err := svc.Enrol(t.Context(), tenantID, advanced, actorFor(learner), enroll.SourceSelf); !errors.Is(err, enroll.ErrPrerequisitesUnmet) {
		t.Errorf("re-enrol = %v, want ErrPrerequisitesUnmet", err)
	}
}

// A course with no prerequisites is not gated, and costs one extra query to
// discover that. The check must not become a reason to avoid the feature.
func TestEnrolIsUnaffectedWhenThereAreNoPrerequisites(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)

	slug, _ := seedCourse(t, db, tenantID, 1, true)

	if _, err := svc.Enrol(t.Context(), tenantID, slug, actorFor(learner), enroll.SourceSelf); err != nil {
		t.Errorf("enrol: %v", err)
	}
}
