package library

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PostgresRepository is the Postgres-backed Repository.
type PostgresRepository struct{}

// NewPostgresRepository returns a PostgresRepository.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

func isForeignKeyViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23503"
}

const bookColumns = `id, title, author, isbn, category, total_copies, available_copies, created_at, updated_at`

func (r *PostgresRepository) AddBook(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewBook) (Book, error) {
	rows, err := tx.Query(ctx,
		`INSERT INTO library_books (tenant_id, title, author, isbn, category, total_copies, available_copies)
		 VALUES ($1, $2, $3, $4, $5, $6, $6)
		 RETURNING `+bookColumns,
		tenantID, n.Title, n.Author, n.ISBN, n.Category, n.TotalCopies)
	if err != nil {
		return Book{}, fmt.Errorf("library: add book: %w", err)
	}
	b, err := pgx.CollectExactlyOneRow(rows, scanBook)
	if err != nil {
		return Book{}, fmt.Errorf("library: add book: %w", err)
	}
	return b, nil
}

// Two statements, not one with an `OR $x IS NULL` predicate: each filter shape gets
// the index that covers it, and neither leaves a Sort node on the request path.
const booksAllSQL = `
	SELECT ` + bookColumns + ` FROM library_books
	WHERE tenant_id = $1
	  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4::uuid))
	ORDER BY created_at DESC, id DESC LIMIT $2`

const booksByCategorySQL = `
	SELECT ` + bookColumns + ` FROM library_books
	WHERE tenant_id = $1 AND category = $5
	  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4::uuid))
	ORDER BY created_at DESC, id DESC LIMIT $2`

func (r *PostgresRepository) Books(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f BookFilter, after *cursor, limit int) ([]Book, error) {
	var afterTime, afterID any
	if after != nil {
		afterTime, afterID = after.CreatedAt, after.ID
	}

	var rows pgx.Rows
	var err error
	if f.Category != "" {
		rows, err = tx.Query(ctx, booksByCategorySQL, tenantID, limit, afterTime, afterID, f.Category)
	} else {
		rows, err = tx.Query(ctx, booksAllSQL, tenantID, limit, afterTime, afterID)
	}
	if err != nil {
		return nil, fmt.Errorf("library: books: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanBook)
}

const loanColumns = `id, book_id, student_id, borrowed_at, due_at, returned_at, status, created_at, updated_at`

// IssueLoan draws one copy and records the loan. The decrement's `available_copies
// > 0` guard and the insert run in one transaction, so the last copy cannot be lent
// twice: whichever issue commits first leaves the other with nothing to draw.
func (r *PostgresRepository) IssueLoan(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewLoan) (Loan, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE library_books SET available_copies = available_copies - 1, updated_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND available_copies > 0`,
		tenantID, n.BookID)
	if err != nil {
		return Loan{}, fmt.Errorf("library: draw copy: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Loan{}, r.absentBookOrNoCopies(ctx, tx, tenantID, n.BookID)
	}

	rows, err := tx.Query(ctx,
		`INSERT INTO library_loans (tenant_id, book_id, student_id, due_at)
		 VALUES ($1, $2, $3, $4)
		 RETURNING `+loanColumns,
		tenantID, n.BookID, n.StudentID, n.DueAt)
	if err != nil {
		if isForeignKeyViolation(err) {
			return Loan{}, ErrNotFound
		}
		return Loan{}, fmt.Errorf("library: issue loan: %w", err)
	}
	l, err := pgx.CollectExactlyOneRow(rows, scanLoan)
	if isForeignKeyViolation(err) {
		return Loan{}, ErrNotFound
	}
	if err != nil {
		return Loan{}, fmt.Errorf("library: issue loan: %w", err)
	}
	return l, nil
}

// ReturnLoan closes an outstanding loan and returns its copy. The `status = 'out'`
// guard is the idempotency: a second return finds nothing and reports it.
func (r *PostgresRepository) ReturnLoan(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Loan, error) {
	rows, err := tx.Query(ctx,
		`UPDATE library_loans SET status = 'returned', returned_at = now(), updated_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND status = 'out'
		 RETURNING `+loanColumns,
		tenantID, id)
	if err != nil {
		return Loan{}, fmt.Errorf("library: return loan: %w", err)
	}
	l, err := pgx.CollectExactlyOneRow(rows, scanLoan)
	if errors.Is(err, pgx.ErrNoRows) {
		return Loan{}, r.absentLoanOrReturned(ctx, tx, tenantID, id)
	}
	if err != nil {
		return Loan{}, fmt.Errorf("library: return loan: %w", err)
	}

	_, err = tx.Exec(ctx,
		`UPDATE library_books
		 SET available_copies = LEAST(available_copies + 1, total_copies), updated_at = now()
		 WHERE tenant_id = $1 AND id = $2`,
		tenantID, l.BookID)
	if err != nil {
		return Loan{}, fmt.Errorf("library: restore copy: %w", err)
	}
	return l, nil
}

const loansAllSQL = `
	SELECT ` + loanColumns + ` FROM library_loans
	WHERE tenant_id = $1
	  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4::uuid))
	  AND ($5::text = '' OR status = $5)
	ORDER BY created_at DESC, id DESC LIMIT $2`

const loansByStudentSQL = `
	SELECT ` + loanColumns + ` FROM library_loans
	WHERE tenant_id = $1 AND student_id = $6
	  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4::uuid))
	  AND ($5::text = '' OR status = $5)
	ORDER BY created_at DESC, id DESC LIMIT $2`

func (r *PostgresRepository) Loans(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f LoanFilter, after *cursor, limit int) ([]Loan, error) {
	var afterTime, afterID any
	if after != nil {
		afterTime, afterID = after.CreatedAt, after.ID
	}

	var rows pgx.Rows
	var err error
	if f.StudentID != nil {
		rows, err = tx.Query(ctx, loansByStudentSQL, tenantID, limit, afterTime, afterID, f.Status, *f.StudentID)
	} else {
		rows, err = tx.Query(ctx, loansAllSQL, tenantID, limit, afterTime, afterID, f.Status)
	}
	if err != nil {
		return nil, fmt.Errorf("library: loans: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanLoan)
}

// absentBookOrNoCopies distinguishes a book that does not exist from one whose
// copies are all out, so the caller can 404 the first and 409 the second.
func (r *PostgresRepository) absentBookOrNoCopies(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT true FROM library_books WHERE tenant_id = $1 AND id = $2`, tenantID, id).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("library: check book: %w", err)
	}
	return ErrNoCopies
}

// absentLoanOrReturned distinguishes a loan that does not exist from one already
// returned, so the caller can 404 the first and 409 the second.
func (r *PostgresRepository) absentLoanOrReturned(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT true FROM library_loans WHERE tenant_id = $1 AND id = $2`, tenantID, id).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("library: check loan: %w", err)
	}
	return ErrAlreadyReturned
}

func scanBook(row pgx.CollectableRow) (Book, error) {
	var b Book
	err := row.Scan(&b.ID, &b.Title, &b.Author, &b.ISBN, &b.Category,
		&b.TotalCopies, &b.AvailableCopies, &b.CreatedAt, &b.UpdatedAt)
	return b, err
}

func scanLoan(row pgx.CollectableRow) (Loan, error) {
	var l Loan
	err := row.Scan(&l.ID, &l.BookID, &l.StudentID, &l.BorrowedAt, &l.DueAt,
		&l.ReturnedAt, &l.Status, &l.CreatedAt, &l.UpdatedAt)
	return l, err
}
