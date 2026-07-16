package admissions_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/admissions"
	"github.com/ebnsina/muallim-api/internal/platform/database"
)

func testDB(t *testing.T) *database.DB {
	t.Helper()
	url := os.Getenv("MUALLIM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MUALLIM_TEST_DATABASE_URL is not set")
	}
	db, err := database.New(t.Context(), database.Options{URL: url, MaxConns: 4},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func seedTenant(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	sub := "a" + id.String()[:8]
	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, subdomain, name) VALUES ($1, $2, $3)`, id, sub, "Test "+sub)
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

func seedClass(t *testing.T, db *database.DB, tenant uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := db.WithTenant(t.Context(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO grade_levels (tenant_id, name, rank) VALUES ($1, 'Class 1', 1) RETURNING id`, tenant).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed class: %v", err)
	}
	return id
}

func seedStudent(t *testing.T, db *database.DB, tenant, class uuid.UUID, admissionNo, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := db.WithTenant(t.Context(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO students (tenant_id, admission_no, full_name, grade_level_id, status)
			 VALUES ($1, $2, $3, $4, 'active') RETURNING id`, tenant, admissionNo, name, class).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed student: %v", err)
	}
	return id
}

type stubAuditor struct{}

func (stubAuditor) Record(context.Context, pgx.Tx, uuid.UUID, admissions.AuditEntry) error {
	return nil
}

func newService(db *database.DB) *admissions.Service {
	return admissions.NewService(db, admissions.NewPostgresRepository(), stubAuditor{})
}

func TestAdmissionsFlow(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := admissions.Author{UserID: uuid.New()}
	class := seedClass(t, db, tenant)

	// An empty applicant name is refused.
	if _, err := svc.Submit(t.Context(), tenant, admissions.NewApplication{}, author); !errors.Is(err, admissions.ErrInvalidApplication) {
		t.Fatalf("an empty application was accepted: %v", err)
	}

	// Submitting two applications puts them in the pending queue, newest first.
	first, err := svc.Submit(t.Context(), tenant, admissions.NewApplication{
		ApplicantName: "Amina Rahman", GuardianName: "Rahman", GuardianPhone: "01700000000", GradeLevelID: &class,
	}, author)
	if err != nil {
		t.Fatalf("submit first: %v", err)
	}
	if first.Status != admissions.StatusPending {
		t.Fatalf("status %q, want pending", first.Status)
	}
	second, err := svc.Submit(t.Context(), tenant, admissions.NewApplication{ApplicantName: "Bilal Ahmed"}, author)
	if err != nil {
		t.Fatalf("submit second: %v", err)
	}

	// The unfiltered listing returns both, newest first.
	all, err := svc.List(t.Context(), tenant, admissions.Filter{}, admissions.PageParams{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all.Applications) != 2 || all.Applications[0].ID != second.ID {
		t.Fatalf("listed %d applications, want 2 newest-first", len(all.Applications))
	}

	// Get reads one back.
	got, err := svc.Get(t.Context(), tenant, first.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ApplicantName != "Amina Rahman" {
		t.Fatalf("got %q", got.ApplicantName)
	}

	// Accepting a pending application sets accepted + decided_at.
	accepted, err := svc.Accept(t.Context(), tenant, first.ID, author)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if accepted.Status != admissions.StatusAccepted || accepted.DecidedAt == nil {
		t.Fatalf("accept left %+v, want accepted with decided_at", accepted)
	}

	// Accepting again is refused — it is no longer pending.
	if _, err := svc.Accept(t.Context(), tenant, first.ID, author); !errors.Is(err, admissions.ErrNotPending) {
		t.Fatalf("re-accept was allowed: %v", err)
	}

	// The status filter narrows to accepted only.
	acceptedList, err := svc.List(t.Context(), tenant, admissions.Filter{Status: admissions.StatusAccepted}, admissions.PageParams{})
	if err != nil {
		t.Fatalf("list accepted: %v", err)
	}
	if len(acceptedList.Applications) != 1 || acceptedList.Applications[0].ID != first.ID {
		t.Fatalf("accepted filter returned %d, want just the accepted one", len(acceptedList.Applications))
	}

	// Rejecting the second (still pending) sets rejected.
	rejected, err := svc.Reject(t.Context(), tenant, second.ID, author)
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if rejected.Status != admissions.StatusRejected {
		t.Fatalf("status %q, want rejected", rejected.Status)
	}
	// Rejecting a non-pending application is refused.
	if _, err := svc.Reject(t.Context(), tenant, second.ID, author); !errors.Is(err, admissions.ErrNotPending) {
		t.Fatalf("re-reject was allowed: %v", err)
	}

	// MarkAdmitted only works from accepted: the rejected one cannot be admitted.
	student := seedStudent(t, db, tenant, class, "2026-001", "Amina Rahman")
	if _, err := svc.MarkAdmitted(t.Context(), tenant, second.ID, student, author); !errors.Is(err, admissions.ErrNotPending) {
		t.Fatalf("admitting a rejected application was allowed: %v", err)
	}

	// The accepted one is admitted: status='admitted' + student_id set.
	admitted, err := svc.MarkAdmitted(t.Context(), tenant, first.ID, student, author)
	if err != nil {
		t.Fatalf("mark admitted: %v", err)
	}
	if admitted.Status != admissions.StatusAdmitted || admitted.StudentID == nil || *admitted.StudentID != student {
		t.Fatalf("admit left %+v, want admitted with the student id", admitted)
	}
	// Admitting again is refused — no longer accepted.
	if _, err := svc.MarkAdmitted(t.Context(), tenant, first.ID, student, author); !errors.Is(err, admissions.ErrNotPending) {
		t.Fatalf("re-admit was allowed: %v", err)
	}

	// An unknown id is not found.
	if _, err := svc.Get(t.Context(), tenant, uuid.New()); !errors.Is(err, admissions.ErrNotFound) {
		t.Fatalf("unknown application: %v", err)
	}
	if _, err := svc.Accept(t.Context(), tenant, uuid.New(), author); !errors.Is(err, admissions.ErrNotFound) {
		t.Fatalf("accepting an unknown application: %v", err)
	}
}
