package ledger_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/ledger"
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
	sub := "l" + id.String()[:8]
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

type stubAuditor struct{}

func (stubAuditor) Record(context.Context, pgx.Tx, uuid.UUID, ledger.AuditEntry) error { return nil }

func newService(db *database.DB) *ledger.Service {
	return ledger.NewService(db, ledger.NewPostgresRepository(), stubAuditor{})
}

func date(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse date %q: %v", s, err)
	}
	return d
}

func TestLedgerFlow(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := ledger.Author{UserID: uuid.New()}

	// An income head and an expense head.
	tuition, err := svc.CreateCategory(t.Context(), tenant, ledger.NewCategory{
		Name: "Tuition income", Kind: ledger.KindIncome,
	}, author)
	if err != nil {
		t.Fatalf("create income category: %v", err)
	}
	salaries, err := svc.CreateCategory(t.Context(), tenant, ledger.NewCategory{
		Name: "Salaries", Kind: ledger.KindExpense,
	}, author)
	if err != nil {
		t.Fatalf("create expense category: %v", err)
	}

	cats, err := svc.ListCategories(t.Context(), tenant)
	if err != nil {
		t.Fatalf("list categories: %v", err)
	}
	if len(cats) != 2 {
		t.Fatalf("listed %d categories, want 2", len(cats))
	}

	// Entries default to BDT poisha.
	in1, err := svc.RecordEntry(t.Context(), tenant, ledger.NewEntry{
		CategoryID: tuition.ID, Amount: 500000, OccurredOn: date(t, "2026-01-10"), Description: "January fees",
	}, author)
	if err != nil {
		t.Fatalf("record income: %v", err)
	}
	if in1.Currency != "BDT" {
		t.Fatalf("currency %q, want the BDT default", in1.Currency)
	}
	if _, err := svc.RecordEntry(t.Context(), tenant, ledger.NewEntry{
		CategoryID: tuition.ID, Amount: 300000, OccurredOn: date(t, "2026-02-10"),
	}, author); err != nil {
		t.Fatalf("record income 2: %v", err)
	}
	if _, err := svc.RecordEntry(t.Context(), tenant, ledger.NewEntry{
		CategoryID: salaries.ID, Amount: 200000, OccurredOn: date(t, "2026-01-31"), Description: "January payroll",
	}, author); err != nil {
		t.Fatalf("record expense: %v", err)
	}

	// The unfiltered board returns every entry, newest first.
	all, err := svc.ListEntries(t.Context(), tenant, ledger.EntryFilter{}, ledger.PageParams{})
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	if len(all.Entries) != 3 {
		t.Fatalf("listed %d entries, want 3", len(all.Entries))
	}
	if !all.Entries[0].OccurredOn.After(all.Entries[len(all.Entries)-1].OccurredOn) {
		t.Fatalf("entries are not newest first")
	}

	// A kind filter narrows to income.
	income, err := svc.ListEntries(t.Context(), tenant, ledger.EntryFilter{Kind: ledger.KindIncome}, ledger.PageParams{})
	if err != nil {
		t.Fatalf("list income: %v", err)
	}
	if len(income.Entries) != 2 {
		t.Fatalf("listed %d income entries, want 2", len(income.Entries))
	}

	// A category filter narrows to that head.
	bySalary, err := svc.ListEntries(t.Context(), tenant, ledger.EntryFilter{CategoryID: &salaries.ID}, ledger.PageParams{})
	if err != nil {
		t.Fatalf("list by category: %v", err)
	}
	if len(bySalary.Entries) != 1 {
		t.Fatalf("listed %d salary entries, want 1", len(bySalary.Entries))
	}

	// A date range excludes February.
	from := date(t, "2026-01-01")
	to := date(t, "2026-01-31")
	january, err := svc.ListEntries(t.Context(), tenant, ledger.EntryFilter{From: &from, To: &to}, ledger.PageParams{})
	if err != nil {
		t.Fatalf("list january: %v", err)
	}
	if len(january.Entries) != 2 {
		t.Fatalf("listed %d January entries, want 2", len(january.Entries))
	}

	// Pagination: one per page walks the whole board.
	first, err := svc.ListEntries(t.Context(), tenant, ledger.EntryFilter{}, ledger.PageParams{Limit: 1})
	if err != nil {
		t.Fatalf("list page 1: %v", err)
	}
	if len(first.Entries) != 1 || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("page 1 is %+v, want one row with a cursor and more", first)
	}
	second, err := svc.ListEntries(t.Context(), tenant, ledger.EntryFilter{}, ledger.PageParams{Limit: 1, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("list page 2: %v", err)
	}
	if len(second.Entries) != 1 || second.Entries[0].ID == first.Entries[0].ID {
		t.Fatalf("page 2 repeated a row: %+v", second)
	}

	// The summary totals income, expense and net per currency.
	summary, err := svc.Summarise(t.Context(), tenant, ledger.EntryFilter{})
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if len(summary.Totals) != 1 {
		t.Fatalf("summary has %d currencies, want 1", len(summary.Totals))
	}
	got := summary.Totals[0]
	if got.Currency != "BDT" || got.Income != 800000 || got.Expense != 200000 || got.Net != 600000 {
		t.Fatalf("summary is %+v, want 800000 in, 200000 out, 600000 net BDT", got)
	}
}

func TestInvalidInputsAreRefused(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := ledger.Author{UserID: uuid.New()}

	// A category with an unknown kind is refused before it reaches the database.
	if _, err := svc.CreateCategory(t.Context(), tenant, ledger.NewCategory{Name: "Odd", Kind: "asset"}, author); !errors.Is(err, ledger.ErrInvalidCategory) {
		t.Fatalf("a bad kind was accepted: %v", err)
	}

	cat, err := svc.CreateCategory(t.Context(), tenant, ledger.NewCategory{Name: "Fees", Kind: ledger.KindIncome}, author)
	if err != nil {
		t.Fatalf("create category: %v", err)
	}

	// A negative amount is refused.
	if _, err := svc.RecordEntry(t.Context(), tenant, ledger.NewEntry{
		CategoryID: cat.ID, Amount: -1, OccurredOn: date(t, "2026-01-10"),
	}, author); !errors.Is(err, ledger.ErrInvalidEntry) {
		t.Fatalf("a negative amount was accepted: %v", err)
	}

	// An entry against a category that does not exist is a 404, not a 500.
	if _, err := svc.RecordEntry(t.Context(), tenant, ledger.NewEntry{
		CategoryID: uuid.New(), Amount: 1000, OccurredOn: date(t, "2026-01-10"),
	}, author); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("an entry against an unknown category was accepted: %v", err)
	}

	// A malformed cursor is a 422, not a 500.
	if _, err := svc.ListEntries(t.Context(), tenant, ledger.EntryFilter{}, ledger.PageParams{Cursor: "not-base64!!"}); !errors.Is(err, ledger.ErrInvalidPage) {
		t.Fatalf("a bad cursor was accepted: %v", err)
	}
}
