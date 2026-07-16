package enroll_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/audit"
	"github.com/ebnsina/muallim-api/internal/enroll"
	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// captureAnnouncer records what enrolment tells the workspace's rules about, so a
// test can assert every way onto a course reaches them — and that no way reaches
// them twice.
type captureAnnouncer struct {
	enrolled  []uuid.UUID // course ids somebody joined
	completed []uuid.UUID
}

func (a *captureAnnouncer) Enrolled(_ context.Context, _ pgx.Tx, _, _, courseID uuid.UUID) error {
	a.enrolled = append(a.enrolled, courseID)
	return nil
}

func (a *captureAnnouncer) CourseCompleted(_ context.Context, _ pgx.Tx, _, _, courseID uuid.UUID) error {
	a.completed = append(a.completed, courseID)
	return nil
}

// courseIDFor resolves a seeded course's id — the grant paths take ids where the
// enrol path takes a slug.
func courseIDFor(t *testing.T, db *database.DB, tenantID uuid.UUID, slug string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := db.WithTenantReadOnly(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM courses WHERE tenant_id = $1 AND slug = $2`, tenantID, slug).Scan(&id)
	}); err != nil {
		t.Fatalf("course id for %q: %v", slug, err)
	}
	return id
}

func newServiceWithAnnouncer(db *database.DB, a enroll.Announcer) *enroll.Service {
	return enroll.NewService(db, enroll.NewPostgresRepository(), enrolAuditor{audit.NewRecorder()},
		newIssuer(db), nil).WithAnnouncer(a)
}

/*
Every way onto a course announces it.

Self-enrolment did, and nothing else did — so a learner who bought a course, was
granted a seat, or took a bundle was the one learner nobody welcomed, and the
purchase was the case that mattered most. The paths are enumerated here because
the next one added will be added the same way: by calling repo.Enrol and
forgetting this.
*/
func TestEveryWayOntoACourseAnnouncesIt(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)

	t.Run("self-enrolment", func(t *testing.T) {
		announcer := &captureAnnouncer{}
		svc := newServiceWithAnnouncer(db, announcer)
		slug, _ := seedCourse(t, db, tenantID, 1, true)

		if _, err := svc.Enrol(t.Context(), tenantID, slug, enroll.Actor{UserID: seedUser(t, db, tenantID)}, enroll.SourceSelf); err != nil {
			t.Fatalf("enrol: %v", err)
		}
		if len(announcer.enrolled) != 1 {
			t.Fatalf("self-enrolment announced %d times, want 1", len(announcer.enrolled))
		}
	})

	t.Run("a granted seat", func(t *testing.T) {
		announcer := &captureAnnouncer{}
		svc := newServiceWithAnnouncer(db, announcer)
		slug, _ := seedCourse(t, db, tenantID, 1, true)

		if _, err := svc.Grant(t.Context(), tenantID, slug, learner, enroll.Actor{UserID: seedUser(t, db, tenantID)}); err != nil {
			t.Fatalf("grant: %v", err)
		}
		if len(announcer.enrolled) != 1 {
			t.Fatalf("a granted seat announced %d times, want 1", len(announcer.enrolled))
		}
	})

	// The one that was silent and mattered most: commerce enrols the buyer through
	// GrantInTx when the money lands.
	t.Run("a purchase", func(t *testing.T) {
		announcer := &captureAnnouncer{}
		svc := newServiceWithAnnouncer(db, announcer)
		slug, _ := seedCourse(t, db, tenantID, 1, true)
		courseID := courseIDFor(t, db, tenantID, slug)
		buyer := seedUser(t, db, tenantID)

		err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return svc.GrantInTx(ctx, tx, tenantID, courseID, buyer, enroll.SourcePurchase)
		})
		if err != nil {
			t.Fatalf("purchase: %v", err)
		}
		if len(announcer.enrolled) != 1 {
			t.Fatalf("a purchase announced %d times, want 1 — the learner paid and heard nothing", len(announcer.enrolled))
		}

		// A gateway delivers the same event twice. The second enrols nobody again, so
		// it must welcome nobody again either.
		err = db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return svc.GrantInTx(ctx, tx, tenantID, courseID, buyer, enroll.SourcePurchase)
		})
		if err != nil {
			t.Fatalf("redelivery: %v", err)
		}
		if len(announcer.enrolled) != 1 {
			t.Fatalf("a redelivered webhook welcomed the buyer %d times", len(announcer.enrolled))
		}
	})

	t.Run("a bundle", func(t *testing.T) {
		announcer := &captureAnnouncer{}
		svc := newServiceWithAnnouncer(db, announcer)
		slugA, _ := seedCourse(t, db, tenantID, 1, true)
		slugB, _ := seedCourse(t, db, tenantID, 1, true)
		ids := []uuid.UUID{courseIDFor(t, db, tenantID, slugA), courseIDFor(t, db, tenantID, slugB)}

		if err := svc.GrantCourses(t.Context(), tenantID, ids, seedUser(t, db, tenantID), enroll.SourceGranted); err != nil {
			t.Fatalf("bundle: %v", err)
		}
		// Two courses is two enrolments, so two notes.
		if len(announcer.enrolled) != 2 {
			t.Fatalf("a two-course bundle announced %d times, want 2", len(announcer.enrolled))
		}
	})
}

// A learner re-clicking Enrol is not an event, and a workspace that welcomed them
// twice is one nobody trusts.
func TestReEnrollingAnnouncesNothing(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenantID := seedTenant(t, db)
	announcer := &captureAnnouncer{}
	svc := newServiceWithAnnouncer(db, announcer)
	slug, _ := seedCourse(t, db, tenantID, 1, true)
	actor := enroll.Actor{UserID: seedUser(t, db, tenantID)}

	if _, err := svc.Enrol(t.Context(), tenantID, slug, actor, enroll.SourceSelf); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := svc.Enrol(t.Context(), tenantID, slug, actor, enroll.SourceSelf); err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(announcer.enrolled) != 1 {
		t.Fatalf("re-enrolling announced %d times, want 1", len(announcer.enrolled))
	}
}
