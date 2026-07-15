package overview_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/overview"
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
	sub := "o" + id.String()[:8]
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

func TestSnapshotCountsTheInstitution(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)

	// Seed a class, two active students and an inactive one, a subject, a staff
	// member, an unpaid fee, and today's attendance for one student.
	var class, present uuid.UUID
	err := db.WithTenant(t.Context(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO grade_levels (tenant_id, name, rank) VALUES ($1,'Class 6',6) RETURNING id`, tenant).Scan(&class); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO students (tenant_id, admission_no, full_name, grade_level_id, status) VALUES ($1,'s1','A',$2,'active') RETURNING id`, tenant, class).Scan(&present); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO students (tenant_id, admission_no, full_name, status) VALUES ($1,'s2','B','active')`, tenant); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO students (tenant_id, admission_no, full_name, status) VALUES ($1,'s3','C','graduated')`, tenant); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO subjects (tenant_id, name) VALUES ($1,'Bangla')`, tenant); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO staff (tenant_id, full_name, role, status) VALUES ($1,'Karim','teacher','active')`, tenant); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO fee_invoices (tenant_id, student_id, title, amount, currency, status) VALUES ($1,$2,'Tuition',200000,'BDT','unpaid')`, tenant, present); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO attendance (tenant_id, student_id, on_date, status) VALUES ($1,$2,CURRENT_DATE,'present')`, tenant, present)
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := overview.NewService(db, overview.NewPostgresRepository())
	snap, err := svc.Snapshot(t.Context(), tenant)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	if snap.Students != 2 {
		t.Errorf("students = %d, want 2 active (the graduated one is excluded)", snap.Students)
	}
	if snap.Staff != 1 || snap.Classes != 1 || snap.Subjects != 1 {
		t.Errorf("staff/classes/subjects = %d/%d/%d, want 1/1/1", snap.Staff, snap.Classes, snap.Subjects)
	}
	if snap.AttendanceToday.Present != 1 || snap.AttendanceToday.Total != 1 {
		t.Errorf("attendance today = %+v, want one present", snap.AttendanceToday)
	}
	if snap.OutstandingFees["BDT"] != 200000 {
		t.Errorf("outstanding BDT = %d, want 200000", snap.OutstandingFees["BDT"])
	}
}
