package payroll_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/payroll"
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
	sub := "p" + id.String()[:8]
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

func seedStaff(t *testing.T, db *database.DB, tenant uuid.UUID, staffNo, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := db.WithTenant(t.Context(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO staff (tenant_id, staff_no, full_name, role, status)
			 VALUES ($1, $2, $3, 'teacher', 'active') RETURNING id`, tenant, staffNo, name).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed staff: %v", err)
	}
	return id
}

type stubAuditor struct{}

func (stubAuditor) Record(context.Context, pgx.Tx, uuid.UUID, payroll.AuditEntry) error { return nil }

func newService(db *database.DB) *payroll.Service {
	return payroll.NewService(db, payroll.NewPostgresRepository(), stubAuditor{})
}

func TestPayrollFlow(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := payroll.Author{UserID: uuid.New()}

	amina := seedStaff(t, db, tenant, "T-001", "Amina Rahman")
	bilal := seedStaff(t, db, tenant, "T-002", "Bilal Ahmed")

	// Amina's salary defaults to BDT poisha: basic 40000, allowances 5000, less 2000
	// deductions. Net is 43000.
	sal, err := svc.SetSalary(t.Context(), tenant, payroll.NewSalary{
		StaffID: amina, BasicAmount: 40000, AllowancesAmount: 5000, DeductionsAmount: 2000,
	}, author)
	if err != nil {
		t.Fatalf("set salary: %v", err)
	}
	if sal.Currency != "BDT" {
		t.Fatalf("currency %q, want the BDT default", sal.Currency)
	}

	// Setting it again replaces the current structure in place — one per staff.
	if _, err := svc.SetSalary(t.Context(), tenant, payroll.NewSalary{
		StaffID: amina, BasicAmount: 42000, AllowancesAmount: 5000, DeductionsAmount: 2000,
	}, author); err != nil {
		t.Fatalf("re-set salary: %v", err)
	}
	got, err := svc.GetSalary(t.Context(), tenant, amina)
	if err != nil {
		t.Fatalf("get salary: %v", err)
	}
	if got.BasicAmount != 42000 {
		t.Fatalf("basic %d, want 42000 after replacement", got.BasicAmount)
	}

	// Bilal also has a salary; a staff member with none is skipped by generation.
	if _, err := svc.SetSalary(t.Context(), tenant, payroll.NewSalary{
		StaffID: bilal, BasicAmount: 30000,
	}, author); err != nil {
		t.Fatalf("set bilal salary: %v", err)
	}

	// Generating January's payroll draws one payslip per staff member with a salary.
	generated, err := svc.GeneratePayslips(t.Context(), tenant, payroll.Batch{Period: "2026-01"}, author)
	if err != nil {
		t.Fatalf("generate payslips: %v", err)
	}
	if generated != 2 {
		t.Fatalf("generated %d payslips, want 2", generated)
	}

	// Re-running the same month pays nobody twice.
	again, err := svc.GeneratePayslips(t.Context(), tenant, payroll.Batch{Period: "2026-01"}, author)
	if err != nil {
		t.Fatalf("re-generate: %v", err)
	}
	if again != 0 {
		t.Fatalf("re-generate produced %d, want 0 — the period must be idempotent", again)
	}

	// Amina's slip nets basic 42000 + allowances 5000 - deductions 2000 = 45000.
	page, err := svc.ListPayslips(t.Context(), tenant, payroll.PayslipFilter{StaffID: &amina}, payroll.PageParams{})
	if err != nil {
		t.Fatalf("list amina payslips: %v", err)
	}
	if len(page.Payslips) != 1 {
		t.Fatalf("listed %d payslips for amina, want 1", len(page.Payslips))
	}
	slip := page.Payslips[0]
	if slip.GrossAmount != 47000 || slip.DeductionsAmount != 2000 || slip.NetAmount != 45000 {
		t.Fatalf("slip is gross=%d ded=%d net=%d, want 47000/2000/45000", slip.GrossAmount, slip.DeductionsAmount, slip.NetAmount)
	}
	if slip.Status != payroll.StatusDraft {
		t.Fatalf("status %q, want draft", slip.Status)
	}

	// The whole workspace's payslip board carries both.
	all, err := svc.ListPayslips(t.Context(), tenant, payroll.PayslipFilter{}, payroll.PageParams{})
	if err != nil {
		t.Fatalf("list payslips: %v", err)
	}
	if len(all.Payslips) != 2 {
		t.Fatalf("listed %d payslips, want 2", len(all.Payslips))
	}

	// Paying a draft settles it; a second payment finds nothing to settle.
	paid, err := svc.MarkPaid(t.Context(), tenant, slip.ID, "bank", author)
	if err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	if paid.Status != payroll.StatusPaid || paid.PaidAt == nil {
		t.Fatalf("status %q paidAt %v, want paid and stamped", paid.Status, paid.PaidAt)
	}
	if _, err := svc.MarkPaid(t.Context(), tenant, slip.ID, "bank", author); !errors.Is(err, payroll.ErrNotDraft) {
		t.Fatalf("a second payment was accepted: %v", err)
	}

	// A status filter narrows the board to the still-draft slip.
	drafts, err := svc.ListPayslips(t.Context(), tenant, payroll.PayslipFilter{Status: payroll.StatusDraft}, payroll.PageParams{})
	if err != nil {
		t.Fatalf("list drafts: %v", err)
	}
	if len(drafts.Payslips) != 1 {
		t.Fatalf("listed %d drafts, want 1", len(drafts.Payslips))
	}
}

func TestGenerateForStaffWithoutSalary(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := payroll.Author{UserID: uuid.New()}

	staffID := seedStaff(t, db, tenant, "T-001", "Amina")

	// Reading a salary that was never set is a not-found.
	if _, err := svc.GetSalary(t.Context(), tenant, staffID); !errors.Is(err, payroll.ErrNotFound) {
		t.Fatalf("get missing salary: %v", err)
	}

	// Generating for that one staff member is refused, not silently empty.
	if _, err := svc.GeneratePayslips(t.Context(), tenant, payroll.Batch{
		Period: "2026-01", StaffID: &staffID,
	}, author); !errors.Is(err, payroll.ErrNoSalary) {
		t.Fatalf("generating for an unpaid staff member: %v", err)
	}

	// A workspace-wide run with nobody on salary simply generates nothing.
	generated, err := svc.GeneratePayslips(t.Context(), tenant, payroll.Batch{Period: "2026-01"}, author)
	if err != nil {
		t.Fatalf("workspace generate: %v", err)
	}
	if generated != 0 {
		t.Fatalf("generated %d, want 0 with nobody on salary", generated)
	}
}

func TestMarkPaidMissingPayslip(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := payroll.Author{UserID: uuid.New()}

	if _, err := svc.MarkPaid(t.Context(), tenant, uuid.New(), "bank", author); !errors.Is(err, payroll.ErrNotFound) {
		t.Fatalf("paying a missing payslip: %v", err)
	}
}

func TestInvalidSalaryRefused(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := payroll.Author{UserID: uuid.New()}
	staffID := seedStaff(t, db, tenant, "T-001", "Amina")

	// Deductions cannot exceed gross pay.
	if _, err := svc.SetSalary(t.Context(), tenant, payroll.NewSalary{
		StaffID: staffID, BasicAmount: 1000, DeductionsAmount: 5000,
	}, author); !errors.Is(err, payroll.ErrInvalidStructure) {
		t.Fatalf("an over-deducted salary was accepted: %v", err)
	}

	// A period-less generation is refused.
	if _, err := svc.GeneratePayslips(t.Context(), tenant, payroll.Batch{Period: ""}, author); !errors.Is(err, payroll.ErrInvalidPayslip) {
		t.Fatalf("a period-less batch was accepted: %v", err)
	}
}
