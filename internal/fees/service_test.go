package fees_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/fees"
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
	sub := "f" + id.String()[:8]
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
			`INSERT INTO grade_levels (tenant_id, name, rank) VALUES ($1, 'Class 6', 6) RETURNING id`, tenant).Scan(&id)
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

func (stubAuditor) Record(context.Context, pgx.Tx, uuid.UUID, fees.AuditEntry) error { return nil }

func newService(db *database.DB) *fees.Service {
	return fees.NewService(db, fees.NewPostgresRepository(), stubAuditor{})
}

func TestFeeFlow(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := fees.Author{UserID: uuid.New()}

	class := seedClass(t, db, tenant)
	amina := seedStudent(t, db, tenant, class, "2025-001", "Amina Rahman")
	seedStudent(t, db, tenant, class, "2025-002", "Bilal Ahmed")

	// A monthly tuition structure defaults to BDT poisha.
	fs, err := svc.CreateStructure(t.Context(), tenant, fees.NewFeeStructure{
		Name: "Monthly Tuition", Amount: 200000, GradeLevelID: &class, Recurrence: fees.Monthly,
	}, author)
	if err != nil {
		t.Fatalf("create structure: %v", err)
	}
	if fs.Currency != "BDT" {
		t.Fatalf("currency %q, want the BDT default", fs.Currency)
	}

	// Billing the class for January raises one invoice per active student.
	issued, err := svc.IssueBatch(t.Context(), tenant, fees.Batch{
		StructureID: fs.ID, Period: "2026-01", GradeLevelID: &class,
	}, author)
	if err != nil {
		t.Fatalf("issue batch: %v", err)
	}
	if issued != 2 {
		t.Fatalf("issued %d, want 2", issued)
	}

	// Re-running the same month bills nobody twice.
	again, err := svc.IssueBatch(t.Context(), tenant, fees.Batch{
		StructureID: fs.ID, Period: "2026-01", GradeLevelID: &class,
	}, author)
	if err != nil {
		t.Fatalf("re-issue: %v", err)
	}
	if again != 0 {
		t.Fatalf("re-issue billed %d, want 0 — the period must be idempotent", again)
	}

	// The unfiltered listing returns every raised invoice — the workspace-wide
	// fee board. A status filter narrows it; neither shape may 500.
	all, err := svc.Invoices(t.Context(), tenant, fees.InvoiceFilter{}, fees.PageParams{})
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	if len(all.Invoices) != 2 {
		t.Fatalf("listed %d invoices, want 2", len(all.Invoices))
	}
	unpaid, err := svc.Invoices(t.Context(), tenant, fees.InvoiceFilter{Status: fees.StatusUnpaid}, fees.PageParams{})
	if err != nil {
		t.Fatalf("list unpaid invoices: %v", err)
	}
	if len(unpaid.Invoices) != 2 {
		t.Fatalf("listed %d unpaid, want 2", len(unpaid.Invoices))
	}

	// Amina's ledger shows one unpaid invoice and the outstanding balance.
	ledger, err := svc.StudentLedger(t.Context(), tenant, amina)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if len(ledger.Invoices) != 1 || ledger.Outstanding["BDT"] != 200000 {
		t.Fatalf("ledger is %+v, want one 200000 BDT due", ledger)
	}

	invoiceID := ledger.Invoices[0].ID

	// Recording a payment settles it; a second payment finds nothing to settle.
	paid, err := svc.RecordPayment(t.Context(), tenant, invoiceID, fees.Payment{Amount: 200000, Method: "bkash"}, author)
	if err != nil {
		t.Fatalf("record payment: %v", err)
	}
	if paid.Status != fees.StatusPaid {
		t.Fatalf("status %q, want paid", paid.Status)
	}
	if _, err := svc.RecordPayment(t.Context(), tenant, invoiceID, fees.Payment{Amount: 200000, Method: "bkash"}, author); !errors.Is(err, fees.ErrNotUnpaid) {
		t.Fatalf("a second payment was accepted: %v", err)
	}

	// After payment, Amina owes nothing.
	ledger, err = svc.StudentLedger(t.Context(), tenant, amina)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if ledger.Outstanding["BDT"] != 0 {
		t.Fatalf("still owes %d BDT after paying", ledger.Outstanding["BDT"])
	}

	// A batch with neither students nor a class is refused.
	if _, err := svc.IssueBatch(t.Context(), tenant, fees.Batch{StructureID: fs.ID, Period: "2026-02"}, author); !errors.Is(err, fees.ErrNoTarget) {
		t.Fatalf("a targetless batch was accepted: %v", err)
	}
}

func TestWaiveOnlyWhenUnpaid(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := fees.Author{UserID: uuid.New()}
	class := seedClass(t, db, tenant)
	student := seedStudent(t, db, tenant, class, "2025-001", "Amina")

	inv, err := svc.IssueInvoice(t.Context(), tenant, fees.NewInvoice{
		StudentID: student, Title: "Exam fee", Amount: 50000,
	}, author)
	if err != nil {
		t.Fatalf("issue invoice: %v", err)
	}

	if _, err := svc.WaiveInvoice(t.Context(), tenant, inv.ID, author); err != nil {
		t.Fatalf("waive: %v", err)
	}
	// A waived invoice cannot then be paid.
	if _, err := svc.RecordPayment(t.Context(), tenant, inv.ID, fees.Payment{Amount: 50000}, author); !errors.Is(err, fees.ErrNotUnpaid) {
		t.Fatalf("a waived invoice was paid: %v", err)
	}
}
