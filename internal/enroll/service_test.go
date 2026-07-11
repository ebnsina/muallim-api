package enroll_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/audit"
	"github.com/ebnsina/lms-api/internal/certify"
	"github.com/ebnsina/lms-api/internal/enroll"
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

type enrolAuditor struct{ recorder *audit.Recorder }

func (a enrolAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e enroll.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID: e.ActorID, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID,
		IP: e.IP, UserAgent: e.UserAgent, Metadata: e.Metadata,
	})
}

func newService(db *database.DB) *enroll.Service {
	return enroll.NewService(db, enroll.NewPostgresRepository(), enrolAuditor{audit.NewRecorder()}, newIssuer(db), nil)
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
			`INSERT INTO users (id, email, password_hash, name) VALUES ($1, $2, 'x', 'U')`,
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

// seedCourse builds a published course with `lessons` lessons, the first of which
// is a preview. Returns the slug and the lesson ids in order.
func seedCourse(t *testing.T, db *database.DB, tenantID uuid.UUID, lessons int, published bool) (string, []uuid.UUID) {
	t.Helper()

	slug := "c-" + uuid.NewString()[:8]
	var ids []uuid.UUID

	status := "draft"
	if published {
		status = "published"
	}

	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var courseID, topicID uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO courses (tenant_id, slug, title, status) VALUES ($1, $2, 'Course', $3) RETURNING id`,
			tenantID, slug, status).Scan(&courseID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO topics (tenant_id, course_id, title, position) VALUES ($1, $2, 'T', 0) RETURNING id`,
			tenantID, courseID).Scan(&topicID); err != nil {
			return err
		}

		for i := range lessons {
			var id uuid.UUID
			if err := tx.QueryRow(ctx,
				`INSERT INTO lessons (tenant_id, topic_id, title, position, is_preview, content, duration_seconds)
				 VALUES ($1, $2, $3, $4, $5, $6, 60) RETURNING id`,
				tenantID, topicID, fmt.Sprintf("Lesson %d", i), i, i == 0, fmt.Sprintf("Body %d", i)).Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed course: %v", err)
	}
	return slug, ids
}

func actorFor(id uuid.UUID) enroll.Actor { return enroll.Actor{UserID: id} }

func TestEnrolThenReadThenComplete(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)
	slug, lessons := seedCourse(t, db, tenantID, 3, true)

	// Before enrolling, the paid lesson is invisible; the preview is not.
	if _, _, err := svc.Lesson(t.Context(), tenantID, lessons[1], enroll.Reader{UserID: learner}); !errors.Is(err, enroll.ErrNotFound) {
		t.Fatalf("a non-enrolled reader saw a paid lesson: %v", err)
	}
	if _, access, err := svc.Lesson(t.Context(), tenantID, lessons[0], enroll.Reader{}); err != nil {
		t.Fatalf("an anonymous reader could not read the preview: %v", err)
	} else if access != enroll.AccessPreview {
		t.Errorf("access = %v, want preview", access)
	}

	enrolment, err := svc.Enrol(t.Context(), tenantID, slug, actorFor(learner), enroll.SourceSelf)
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	if enrolment.Status != enroll.StatusActive {
		t.Errorf("status = %q, want active", enrolment.Status)
	}

	// Now the paid lesson opens.
	lesson, access, err := svc.Lesson(t.Context(), tenantID, lessons[1], enroll.Reader{UserID: learner})
	if err != nil {
		t.Fatalf("Lesson: %v", err)
	}
	if access != enroll.AccessEnrolled {
		t.Errorf("access = %v, want enrolled", access)
	}
	if lesson.Content != "Body 1" {
		t.Errorf("content = %q; the body was not returned", lesson.Content)
	}

	// Complete two of three.
	for _, id := range lessons[:2] {
		if _, err := svc.CompleteLesson(t.Context(), tenantID, id, actorFor(learner), true); err != nil {
			t.Fatalf("CompleteLesson: %v", err)
		}
	}

	progress, err := svc.Progress(t.Context(), tenantID, slug, learner)
	if err != nil {
		t.Fatal(err)
	}
	if progress.LessonsCompleted != 2 || progress.LessonsTotal != 3 || progress.Percent != 66 {
		t.Errorf("progress = %d/%d (%d%%), want 2/3 (66%%)",
			progress.LessonsCompleted, progress.LessonsTotal, progress.Percent)
	}
}

// Finishing the last lesson completes the enrolment, once, and it is audited.
func TestFinishingTheLastLessonCompletesTheEnrolment(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)
	slug, lessons := seedCourse(t, db, tenantID, 2, true)

	if _, err := svc.Enrol(t.Context(), tenantID, slug, actorFor(learner), enroll.SourceSelf); err != nil {
		t.Fatal(err)
	}
	for _, id := range lessons {
		if _, err := svc.CompleteLesson(t.Context(), tenantID, id, actorFor(learner), true); err != nil {
			t.Fatal(err)
		}
	}

	progress, err := svc.Progress(t.Context(), tenantID, slug, learner)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Percent != 100 || !progress.Complete() {
		t.Fatalf("progress = %d%%, want 100", progress.Percent)
	}

	enrolments, err := svc.Enrolments(t.Context(), tenantID, learner, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(enrolments) != 1 {
		t.Fatalf("enrolments = %d, want 1", len(enrolments))
	}
	if enrolments[0].Enrolment.Status != enroll.StatusCompleted {
		t.Errorf("status = %q, want completed", enrolments[0].Enrolment.Status)
	}
	if enrolments[0].Enrolment.CompletedAt == nil {
		t.Error("completed_at was not stamped")
	}

	// A completed learner still reads the course. Finishing is not eviction.
	if _, access, err := svc.Lesson(t.Context(), tenantID, lessons[1], enroll.Reader{UserID: learner}); err != nil {
		t.Errorf("a completed learner lost access: %v", err)
	} else if access != enroll.AccessEnrolled {
		t.Errorf("access = %v, want enrolled", access)
	}

	// Re-completing the last lesson is not a second completion event.
	if _, err := svc.CompleteLesson(t.Context(), tenantID, lessons[1], actorFor(learner), true); err != nil {
		t.Fatal(err)
	}

	var events int
	err = db.WithTenantReadOnly(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = $1`, enroll.ActionCourseFinished).Scan(&events)
	})
	if err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Errorf("course.completed recorded %d times, want 1", events)
	}
}

// Completing a lesson twice must not move the timestamp: when you finished it is
// a fact, and clicking again does not change it.
func TestCompletingTwiceIsIdempotent(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)
	slug, lessons := seedCourse(t, db, tenantID, 3, true)

	if _, err := svc.Enrol(t.Context(), tenantID, slug, actorFor(learner), enroll.SourceSelf); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteLesson(t.Context(), tenantID, lessons[0], actorFor(learner), true); err != nil {
		t.Fatal(err)
	}

	first, _, err := svc.Lesson(t.Context(), tenantID, lessons[0], enroll.Reader{UserID: learner})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.CompleteLesson(t.Context(), tenantID, lessons[0], actorFor(learner), true); err != nil {
		t.Fatal(err)
	}
	second, _, err := svc.Lesson(t.Context(), tenantID, lessons[0], enroll.Reader{UserID: learner})
	if err != nil {
		t.Fatal(err)
	}

	if !first.CompletedAt.Equal(*second.CompletedAt) {
		t.Error("completing a lesson twice moved its completion timestamp")
	}

	progress, err := svc.Progress(t.Context(), tenantID, slug, learner)
	if err != nil {
		t.Fatal(err)
	}
	if progress.LessonsCompleted != 1 {
		t.Errorf("completed = %d, want 1 — a double completion was double-counted", progress.LessonsCompleted)
	}
}

func TestReopeningALessonLowersProgress(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)
	slug, lessons := seedCourse(t, db, tenantID, 2, true)

	if _, err := svc.Enrol(t.Context(), tenantID, slug, actorFor(learner), enroll.SourceSelf); err != nil {
		t.Fatal(err)
	}
	for _, id := range lessons {
		if _, err := svc.CompleteLesson(t.Context(), tenantID, id, actorFor(learner), true); err != nil {
			t.Fatal(err)
		}
	}

	progress, err := svc.CompleteLesson(t.Context(), tenantID, lessons[0], actorFor(learner), false)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if progress.LessonsCompleted != 1 || progress.Percent != 50 {
		t.Errorf("progress = %d/%d (%d%%), want 1/2 (50%%)",
			progress.LessonsCompleted, progress.LessonsTotal, progress.Percent)
	}
}

// Reading a free preview is not studying a course.
func TestCompletingRequiresAnEnrolment(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)
	_, lessons := seedCourse(t, db, tenantID, 2, true)

	// The preview is readable...
	if _, _, err := svc.Lesson(t.Context(), tenantID, lessons[0], enroll.Reader{UserID: learner}); err != nil {
		t.Fatalf("preview unreadable: %v", err)
	}
	// ...but not completable.
	if _, err := svc.CompleteLesson(t.Context(), tenantID, lessons[0], actorFor(learner), true); !errors.Is(err, enroll.ErrNotEnrolled) {
		t.Errorf("err = %v, want ErrNotEnrolled", err)
	}
}

// Re-enrolling must reactivate the original row, so progress survives a change of
// mind.
func TestCancelThenReEnrolKeepsProgress(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)
	slug, lessons := seedCourse(t, db, tenantID, 4, true)

	if _, err := svc.Enrol(t.Context(), tenantID, slug, actorFor(learner), enroll.SourceSelf); err != nil {
		t.Fatal(err)
	}
	for _, id := range lessons[:2] {
		if _, err := svc.CompleteLesson(t.Context(), tenantID, id, actorFor(learner), true); err != nil {
			t.Fatal(err)
		}
	}

	if err := svc.Cancel(t.Context(), tenantID, slug, actorFor(learner)); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// Access is gone.
	if _, _, err := svc.Lesson(t.Context(), tenantID, lessons[1], enroll.Reader{UserID: learner}); !errors.Is(err, enroll.ErrNotFound) {
		t.Errorf("a cancelled learner still reads paid lessons: %v", err)
	}

	// Coming back restores both access and progress.
	if _, err := svc.Enrol(t.Context(), tenantID, slug, actorFor(learner), enroll.SourceSelf); err != nil {
		t.Fatalf("re-enrol: %v", err)
	}
	if _, _, err := svc.Lesson(t.Context(), tenantID, lessons[1], enroll.Reader{UserID: learner}); err != nil {
		t.Errorf("a returning learner has no access: %v", err)
	}

	progress, err := svc.Progress(t.Context(), tenantID, slug, learner)
	if err != nil {
		t.Fatal(err)
	}
	if progress.LessonsCompleted != 2 {
		t.Errorf("completed = %d, want 2 — re-enrolling lost the learner's place", progress.LessonsCompleted)
	}

	// And exactly one enrolment row exists.
	var rows int
	err = db.WithTenantReadOnly(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM enrolments WHERE user_id = $1`, learner).Scan(&rows)
	})
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("enrolment rows = %d, want 1 — re-enrolling created a duplicate", rows)
	}
}

// An unpublished course is invisible, not "closed for enrolment".
func TestCannotEnrolInADraft(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)
	slug, lessons := seedCourse(t, db, tenantID, 2, false)

	if _, err := svc.Enrol(t.Context(), tenantID, slug, actorFor(learner), enroll.SourceSelf); !errors.Is(err, enroll.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}

	// Not even the preview lesson of a draft.
	if _, _, err := svc.Lesson(t.Context(), tenantID, lessons[0], enroll.Reader{UserID: learner}); !errors.Is(err, enroll.ErrNotFound) {
		t.Errorf("a draft's preview lesson was readable: %v", err)
	}

	// The author sees it.
	if _, access, err := svc.Lesson(t.Context(), tenantID, lessons[1], enroll.Reader{UserID: learner, CanAuthor: true}); err != nil {
		t.Errorf("an author cannot read their own draft: %v", err)
	} else if access != enroll.AccessAuthor {
		t.Errorf("access = %v, want author", access)
	}
}

// A learner in one workspace must not reach a course in another.
func TestEnrolmentIsTenantScoped(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	acme := seedTenant(t, db)
	globex := seedTenant(t, db)

	learner := seedUser(t, db, acme)
	slug, lessons := seedCourse(t, db, acme, 2, true)

	if _, err := svc.Enrol(t.Context(), globex, slug, actorFor(learner), enroll.SourceSelf); !errors.Is(err, enroll.ErrNotFound) {
		t.Errorf("enrolled in another workspace's course: %v", err)
	}
	if _, _, err := svc.Lesson(t.Context(), globex, lessons[0], enroll.Reader{UserID: learner}); !errors.Is(err, enroll.ErrNotFound) {
		t.Errorf("read another workspace's lesson: %v", err)
	}
}

// The dashboard is one joined query, however many enrolments it returns.
func TestDashboardHasNoNPlusOne(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)

	for range 5 {
		slug, _ := seedCourse(t, db, tenantID, 2, true)
		if _, err := svc.Enrol(t.Context(), tenantID, slug, actorFor(learner), enroll.SourceSelf); err != nil {
			t.Fatal(err)
		}
	}

	counter := &database.Counter{}
	ctx := database.WithCounter(t.Context(), counter)

	enrolments, err := svc.Enrolments(ctx, tenantID, learner, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(enrolments) != 5 {
		t.Fatalf("enrolments = %d, want 5", len(enrolments))
	}
	if got := counter.Count(); got != 1 {
		t.Errorf("the dashboard issued %d queries for 5 enrolments, want 1 — course and progress must be joined", got)
	}
}

// Reading a lesson decides access, loads the body, and reports the reader's own
// progress. One query, on the hottest path in the product.
func TestLessonReadIsOneQuery(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID)
	slug, lessons := seedCourse(t, db, tenantID, 3, true)

	if _, err := svc.Enrol(t.Context(), tenantID, slug, actorFor(learner), enroll.SourceSelf); err != nil {
		t.Fatal(err)
	}

	counter := &database.Counter{}
	ctx := database.WithCounter(t.Context(), counter)

	if _, _, err := svc.Lesson(ctx, tenantID, lessons[1], enroll.Reader{UserID: learner}); err != nil {
		t.Fatal(err)
	}
	if got := counter.Count(); got != 1 {
		t.Errorf("reading a lesson issued %d queries, want 1 — the access check must not be a second round trip", got)
	}
}

func TestGrantEnrolsSomebodyElse(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	instructor := seedUser(t, db, tenantID)
	learner := seedUser(t, db, tenantID)

	// A grant may target an unpublished course: previewing with a colleague is why
	// grants exist.
	slug, lessons := seedCourse(t, db, tenantID, 2, false)

	enrolment, err := svc.Grant(t.Context(), tenantID, slug, learner, actorFor(instructor))
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if enrolment.Source != enroll.SourceGranted {
		t.Errorf("source = %q, want granted", enrolment.Source)
	}

	// Granting twice is a conflict, not a silent second row.
	if _, err := svc.Grant(t.Context(), tenantID, slug, learner, actorFor(instructor)); !errors.Is(err, enroll.ErrAlreadyEnrolled) {
		t.Errorf("err = %v, want ErrAlreadyEnrolled", err)
	}

	// The audit names the instructor as actor and the learner in metadata.
	var actor uuid.UUID
	err = db.WithTenantReadOnly(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT actor_id FROM audit_log WHERE action = $1 AND metadata->>'learner_id' = $2`,
			enroll.ActionEnrolled, learner.String()).Scan(&actor)
	})
	if err != nil {
		t.Fatal(err)
	}
	if actor != instructor {
		t.Errorf("audit actor = %s, want the granting instructor %s", actor, instructor)
	}

	// The course is still a draft, so the granted learner cannot read it yet.
	if _, _, err := svc.Lesson(t.Context(), tenantID, lessons[1], enroll.Reader{UserID: learner}); !errors.Is(err, enroll.ErrNotFound) {
		t.Errorf("a granted learner read an unpublished lesson: %v", err)
	}
}

/*
The real certificate service, exactly as cmd/ wires it.

A stub would return nil and prove nothing: whether finishing a course issues a
certificate is the question, and `enroll` cannot answer it — it has never heard of
`certify`.
*/
type issuer struct{ svc *certify.Service }

func (i issuer) IssueIfEarned(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) error {
	return i.svc.IssueIfEarned(ctx, tx, tenantID, courseID, userID)
}

type certifyAuditor struct{ recorder *audit.Recorder }

func (a certifyAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e certify.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID: e.ActorID, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID,
		Metadata: e.Metadata,
	})
}

func newIssuer(db *database.DB) issuer {
	return issuer{certify.NewService(db, certify.NewPostgresRepository(), certifyAuditor{audit.NewRecorder()})}
}
