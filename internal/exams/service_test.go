package exams_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/exams"
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
	sub := "e" + id.String()[:8]
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

func seedUser(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			id, "u"+id.String()[:8]+"@example.test")
		return err
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_ = db.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
			return err
		})
	})
	return id
}

// seedStudent and seedSubject write rows academics owns, directly, so the exams
// tests can stand up marks without importing a sibling domain.
func seedStudent(t *testing.T, db *database.DB, tenant uuid.UUID, admissionNo, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := db.WithTenant(t.Context(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO students (tenant_id, admission_no, full_name) VALUES ($1, $2, $3) RETURNING id`,
			tenant, admissionNo, name).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed student: %v", err)
	}
	return id
}

func seedSubject(t *testing.T, db *database.DB, tenant uuid.UUID, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := db.WithTenant(t.Context(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO subjects (tenant_id, name) VALUES ($1, $2) RETURNING id`, tenant, name).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed subject: %v", err)
	}
	return id
}

type stubAuditor struct{}

func (stubAuditor) Record(context.Context, pgx.Tx, uuid.UUID, exams.AuditEntry) error { return nil }

func newService(db *database.DB) *exams.Service {
	return exams.NewService(db, exams.NewPostgresRepository(), stubAuditor{})
}

func TestExamFlow(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := exams.Author{UserID: seedUser(t, db)}

	// The default scale seeds on first ask and is idempotent.
	scale, err := svc.DefaultScale(t.Context(), tenant, author)
	if err != nil {
		t.Fatalf("default scale: %v", err)
	}
	if !scale.IsDefault || len(scale.Bands) != 7 {
		t.Fatalf("default scale is %+v, want the 7-band BD GPA-5", scale)
	}
	again, err := svc.DefaultScale(t.Context(), tenant, author)
	if err != nil || again.ID != scale.ID {
		t.Fatalf("default scale not idempotent: %v (%v vs %v)", err, again.ID, scale.ID)
	}

	student := seedStudent(t, db, tenant, "2025-001", "Amina Rahman")
	bangla := seedSubject(t, db, tenant, "Bangla")
	math := seedSubject(t, db, tenant, "Math")

	exam, err := svc.CreateExam(t.Context(), tenant, exams.NewExam{Name: "First Term 2026", ScaleID: scale.ID}, author)
	if err != nil {
		t.Fatalf("create exam: %v", err)
	}

	entered, err := svc.EnterMarks(t.Context(), tenant, exam.ID, []exams.Mark{
		{StudentID: student, SubjectID: bangla, FullMarks: 100, Obtained: 85},
		{StudentID: student, SubjectID: math, FullMarks: 100, Obtained: 40},
	}, author)
	if err != nil {
		t.Fatalf("enter marks: %v", err)
	}
	if entered != 2 {
		t.Fatalf("entered %d, want 2", entered)
	}

	// Re-entering a subject updates in place rather than stacking a second row.
	if _, err := svc.EnterMarks(t.Context(), tenant, exam.ID, []exams.Mark{
		{StudentID: student, SubjectID: math, FullMarks: 100, Obtained: 60},
	}, author); err != nil {
		t.Fatalf("re-enter mark: %v", err)
	}

	rc, err := svc.ReportCard(t.Context(), tenant, exam.ID, student)
	if err != nil {
		t.Fatalf("report card: %v", err)
	}
	if len(rc.Subjects) != 2 {
		t.Fatalf("%d subjects on the card, want 2 (the re-mark must not stack)", len(rc.Subjects))
	}
	// Bangla 85 → A+/5.0, Math 60 → A-/3.5. GPA = 4.25, both pass.
	if want := (5.0 + 3.5) / 2; rc.GPA != want {
		t.Errorf("GPA %.3f, want %.3f", rc.GPA, want)
	}
	if !rc.Passed {
		t.Error("no failing subject, should pass")
	}

	// Publishing fixes the results: further marks are refused.
	if _, err := svc.PublishExam(t.Context(), tenant, exam.ID, author); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := svc.EnterMarks(t.Context(), tenant, exam.ID, []exams.Mark{
		{StudentID: student, SubjectID: bangla, FullMarks: 100, Obtained: 99},
	}, author); !errors.Is(err, exams.ErrExamPublished) {
		t.Fatalf("marks accepted after publish: %v", err)
	}

	// A student with no marks in the exam has no report card.
	other := seedStudent(t, db, tenant, "2025-002", "Bilal Ahmed")
	if _, err := svc.ReportCard(t.Context(), tenant, exam.ID, other); !errors.Is(err, exams.ErrNotFound) {
		t.Fatalf("a card for an unmarked student should be not-found: %v", err)
	}
}

func TestInvalidMarkRejected(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := exams.Author{UserID: seedUser(t, db)}
	scale, err := svc.DefaultScale(t.Context(), tenant, author)
	if err != nil {
		t.Fatalf("default scale: %v", err)
	}
	exam, err := svc.CreateExam(t.Context(), tenant, exams.NewExam{Name: "Test", ScaleID: scale.ID}, author)
	if err != nil {
		t.Fatalf("create exam: %v", err)
	}
	student := seedStudent(t, db, tenant, "2025-001", "Amina")
	subject := seedSubject(t, db, tenant, "Bangla")

	// Obtained above the full marks is refused before any row is written.
	if _, err := svc.EnterMarks(t.Context(), tenant, exam.ID, []exams.Mark{
		{StudentID: student, SubjectID: subject, FullMarks: 100, Obtained: 120},
	}, author); !errors.Is(err, exams.ErrInvalidMark) {
		t.Fatalf("a mark above full marks was accepted: %v", err)
	}
}
