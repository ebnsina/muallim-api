package library_test

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

	"github.com/ebnsina/muallim-api/internal/library"
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

func seedStudent(t *testing.T, db *database.DB, tenant uuid.UUID, admissionNo, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := db.WithTenant(t.Context(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO students (tenant_id, admission_no, full_name, status)
			 VALUES ($1, $2, $3, 'active') RETURNING id`, tenant, admissionNo, name).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed student: %v", err)
	}
	return id
}

type stubAuditor struct{}

func (stubAuditor) Record(context.Context, pgx.Tx, uuid.UUID, library.AuditEntry) error { return nil }

func newService(db *database.DB) *library.Service {
	return library.NewService(db, library.NewPostgresRepository(), stubAuditor{})
}

func TestLoanFlow(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := library.Author{UserID: uuid.New()}

	amina := seedStudent(t, db, tenant, "2025-001", "Amina Rahman")
	bilal := seedStudent(t, db, tenant, "2025-002", "Bilal Ahmed")

	// A single-copy title.
	category := "Fiqh"
	book, err := svc.AddBook(t.Context(), tenant, library.NewBook{
		Title: "Bidayat al-Mujtahid", Author: "Ibn Rushd", Category: &category, TotalCopies: 1,
	}, author)
	if err != nil {
		t.Fatalf("add book: %v", err)
	}
	if book.AvailableCopies != 1 {
		t.Fatalf("available %d, want 1 free copy at creation", book.AvailableCopies)
	}

	// The catalogue lists it; the category filter finds it too.
	all, err := svc.ListBooks(t.Context(), tenant, library.BookFilter{}, library.PageParams{})
	if err != nil {
		t.Fatalf("list books: %v", err)
	}
	if len(all.Books) != 1 {
		t.Fatalf("listed %d books, want 1", len(all.Books))
	}
	byCat, err := svc.ListBooks(t.Context(), tenant, library.BookFilter{Category: "Fiqh"}, library.PageParams{})
	if err != nil {
		t.Fatalf("list by category: %v", err)
	}
	if len(byCat.Books) != 1 {
		t.Fatalf("category listed %d books, want 1", len(byCat.Books))
	}

	// Issuing the loan draws the only copy.
	due := time.Now().Add(14 * 24 * time.Hour)
	loan, err := svc.IssueLoan(t.Context(), tenant, library.NewLoan{
		BookID: book.ID, StudentID: amina, DueAt: due,
	}, author)
	if err != nil {
		t.Fatalf("issue loan: %v", err)
	}
	if loan.Status != library.StatusOut {
		t.Fatalf("status %q, want out", loan.Status)
	}

	// With no copies left, a second borrower is refused.
	if _, err := svc.IssueLoan(t.Context(), tenant, library.NewLoan{
		BookID: book.ID, StudentID: bilal, DueAt: due,
	}, author); !errors.Is(err, library.ErrNoCopies) {
		t.Fatalf("a loan of the last copy was accepted: %v", err)
	}

	// The loans board shows the one outstanding loan; the status filter narrows it.
	out, err := svc.ListLoans(t.Context(), tenant, library.LoanFilter{Status: library.StatusOut}, library.PageParams{})
	if err != nil {
		t.Fatalf("list loans: %v", err)
	}
	if len(out.Loans) != 1 {
		t.Fatalf("listed %d outstanding loans, want 1", len(out.Loans))
	}
	byStudent, err := svc.ListLoans(t.Context(), tenant, library.LoanFilter{StudentID: &amina}, library.PageParams{})
	if err != nil {
		t.Fatalf("list student loans: %v", err)
	}
	if len(byStudent.Loans) != 1 {
		t.Fatalf("student had %d loans, want 1", len(byStudent.Loans))
	}

	// Returning it puts the copy back.
	returned, err := svc.ReturnLoan(t.Context(), tenant, loan.ID, author)
	if err != nil {
		t.Fatalf("return loan: %v", err)
	}
	if returned.Status != library.StatusReturned || returned.ReturnedAt == nil {
		t.Fatalf("loan not marked returned: %+v", returned)
	}

	// A second return finds nothing to return.
	if _, err := svc.ReturnLoan(t.Context(), tenant, loan.ID, author); !errors.Is(err, library.ErrAlreadyReturned) {
		t.Fatalf("a second return was accepted: %v", err)
	}

	// With the copy back, the book can be lent again.
	if _, err := svc.IssueLoan(t.Context(), tenant, library.NewLoan{
		BookID: book.ID, StudentID: bilal, DueAt: due,
	}, author); err != nil {
		t.Fatalf("re-issue after return: %v", err)
	}
}

func TestIssueUnknownBookIsNotFound(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := library.Author{UserID: uuid.New()}
	student := seedStudent(t, db, tenant, "2025-001", "Amina")

	if _, err := svc.IssueLoan(t.Context(), tenant, library.NewLoan{
		BookID: uuid.New(), StudentID: student, DueAt: time.Now().Add(24 * time.Hour),
	}, author); !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("a loan of a missing book was accepted: %v", err)
	}
}

func TestReturnUnknownLoanIsNotFound(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := library.Author{UserID: uuid.New()}

	if _, err := svc.ReturnLoan(t.Context(), tenant, uuid.New(), author); !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("returning a missing loan succeeded: %v", err)
	}
}
