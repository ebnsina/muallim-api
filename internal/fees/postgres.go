package fees

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

func (r *PostgresRepository) CreateStructure(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewFeeStructure) (FeeStructure, error) {
	var fs FeeStructure
	err := tx.QueryRow(ctx,
		`INSERT INTO fee_structures (tenant_id, name, amount, currency, grade_level_id, recurrence)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, name, amount, currency, grade_level_id, recurrence, created_at`,
		tenantID, n.Name, n.Amount, n.Currency, n.GradeLevelID, n.Recurrence).
		Scan(&fs.ID, &fs.Name, &fs.Amount, &fs.Currency, &fs.GradeLevelID, &fs.Recurrence, &fs.CreatedAt)
	if err != nil {
		if isForeignKeyViolation(err) {
			return FeeStructure{}, ErrNotFound
		}
		return FeeStructure{}, fmt.Errorf("fees: create structure: %w", err)
	}
	return fs, nil
}

func (r *PostgresRepository) Structures(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit int) ([]FeeStructure, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, name, amount, currency, grade_level_id, recurrence, created_at
		 FROM fee_structures WHERE tenant_id = $1 ORDER BY name, id LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("fees: structures: %w", err)
	}
	return pgx.CollectRows(rows, scanStructure)
}

func (r *PostgresRepository) StructureByID(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (FeeStructure, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, name, amount, currency, grade_level_id, recurrence, created_at
		 FROM fee_structures WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return FeeStructure{}, fmt.Errorf("fees: load structure: %w", err)
	}
	fs, err := pgx.CollectExactlyOneRow(rows, scanStructure)
	if errors.Is(err, pgx.ErrNoRows) {
		return FeeStructure{}, ErrNotFound
	}
	return fs, err
}

func (r *PostgresRepository) DeleteStructure(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error {
	tag, err := tx.Exec(ctx, `DELETE FROM fee_structures WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("fees: delete structure: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

const invoiceColumns = `id, student_id, fee_structure_id, title, amount, currency, period,
	due_date, status, paid_amount, paid_at, method, note, created_at, updated_at`

func (r *PostgresRepository) CreateInvoice(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewInvoice) (Invoice, error) {
	rows, err := tx.Query(ctx,
		`INSERT INTO fee_invoices (tenant_id, student_id, title, amount, currency, due_date, note)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING `+invoiceColumns,
		tenantID, n.StudentID, n.Title, n.Amount, n.Currency, n.DueDate, n.Note)
	if err != nil {
		return Invoice{}, fmt.Errorf("fees: create invoice: %w", err)
	}
	inv, err := pgx.CollectExactlyOneRow(rows, scanInvoice)
	if isForeignKeyViolation(err) {
		return Invoice{}, ErrNotFound
	}
	if err != nil {
		return Invoice{}, fmt.Errorf("fees: create invoice: %w", err)
	}
	return inv, nil
}

// IssueBatch raises one invoice per targeted student for a period, skipping any
// already billed for it. Students named explicitly go through unnest; otherwise a
// whole class's active students are selected. One statement either way.
func (r *PostgresRepository) IssueBatch(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, s FeeStructure, b Batch) (int, error) {
	const insert = `
		INSERT INTO fee_invoices (tenant_id, student_id, fee_structure_id, title, amount, currency, period, due_date)
		SELECT $1, sid, $2, $3, $4, $5, $6, $7 FROM (%s) AS t(sid)
		ON CONFLICT (tenant_id, student_id, fee_structure_id, period) WHERE fee_structure_id IS NOT NULL
		DO NOTHING`

	var (
		source string
		args   = []any{tenantID, s.ID, s.Name, s.Amount, s.Currency, b.Period, b.DueDate}
	)
	if len(b.StudentIDs) > 0 {
		source = "SELECT unnest($8::uuid[])"
		args = append(args, b.StudentIDs)
	} else {
		source = "SELECT id FROM students WHERE tenant_id = $1 AND grade_level_id = $8 AND status = 'active'"
		args = append(args, *b.GradeLevelID)
	}

	tag, err := tx.Exec(ctx, fmt.Sprintf(insert, source), args...)
	if err != nil {
		if isForeignKeyViolation(err) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("fees: issue batch: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Two statements, not one with an `OR $x IS NULL` predicate: each filter shape gets
// the index that covers it, and neither leaves a Sort node on the request path.
const invoicesAllSQL = `
	SELECT ` + invoiceColumns + ` FROM fee_invoices
	WHERE tenant_id = $1
	  AND ($4::timestamptz IS NULL OR (created_at, id) < ($4, $5::uuid))
	  AND ($6::text = '' OR status = $6)
	ORDER BY created_at DESC, id DESC LIMIT $2`

const invoicesByStudentSQL = `
	SELECT ` + invoiceColumns + ` FROM fee_invoices
	WHERE tenant_id = $1 AND student_id = $3
	  AND ($4::timestamptz IS NULL OR (created_at, id) < ($4, $5::uuid))
	  AND ($6::text = '' OR status = $6)
	ORDER BY created_at DESC, id DESC LIMIT $2`

func (r *PostgresRepository) Invoices(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f InvoiceFilter, after *cursor, limit int) ([]Invoice, error) {
	var afterTime, afterID any
	if after != nil {
		afterTime, afterID = after.CreatedAt, after.ID
	}

	var rows pgx.Rows
	var err error
	if f.StudentID != nil {
		rows, err = tx.Query(ctx, invoicesByStudentSQL, tenantID, limit, *f.StudentID, afterTime, afterID, f.Status)
	} else {
		rows, err = tx.Query(ctx, invoicesAllSQL, tenantID, limit, nil, afterTime, afterID, f.Status)
	}
	if err != nil {
		return nil, fmt.Errorf("fees: invoices: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanInvoice)
}

func (r *PostgresRepository) StudentInvoices(ctx context.Context, tx pgx.Tx, tenantID, studentID uuid.UUID) ([]Invoice, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+invoiceColumns+` FROM fee_invoices
		 WHERE tenant_id = $1 AND student_id = $2 ORDER BY created_at DESC, id DESC LIMIT 500`,
		tenantID, studentID)
	if err != nil {
		return nil, fmt.Errorf("fees: student invoices: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanInvoice)
}

func (r *PostgresRepository) RecordPayment(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, p Payment) (Invoice, error) {
	rows, err := tx.Query(ctx,
		`UPDATE fee_invoices
		 SET status = 'paid', paid_amount = $3, paid_at = now(), method = $4,
		     note = CASE WHEN $5 = '' THEN note ELSE $5 END, updated_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND status = 'unpaid'
		 RETURNING `+invoiceColumns,
		tenantID, id, p.Amount, p.Method, p.Note)
	if err != nil {
		return Invoice{}, fmt.Errorf("fees: record payment: %w", err)
	}
	inv, err := pgx.CollectExactlyOneRow(rows, scanInvoice)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, r.absentOrLocked(ctx, tx, tenantID, id)
	}
	return inv, err
}

func (r *PostgresRepository) SetStatus(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, from, to string) (Invoice, error) {
	rows, err := tx.Query(ctx,
		`UPDATE fee_invoices SET status = $4, updated_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND status = $3
		 RETURNING `+invoiceColumns,
		tenantID, id, from, to)
	if err != nil {
		return Invoice{}, fmt.Errorf("fees: set status: %w", err)
	}
	inv, err := pgx.CollectExactlyOneRow(rows, scanInvoice)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, r.absentOrLocked(ctx, tx, tenantID, id)
	}
	return inv, err
}

// absentOrLocked distinguishes an invoice that does not exist from one that is no
// longer unpaid, so the caller can 404 the first and 409 the second.
func (r *PostgresRepository) absentOrLocked(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT true FROM fee_invoices WHERE tenant_id = $1 AND id = $2`, tenantID, id).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("fees: check invoice: %w", err)
	}
	return ErrNotUnpaid
}

func scanStructure(row pgx.CollectableRow) (FeeStructure, error) {
	var fs FeeStructure
	err := row.Scan(&fs.ID, &fs.Name, &fs.Amount, &fs.Currency, &fs.GradeLevelID, &fs.Recurrence, &fs.CreatedAt)
	return fs, err
}

func scanInvoice(row pgx.CollectableRow) (Invoice, error) {
	var i Invoice
	err := row.Scan(&i.ID, &i.StudentID, &i.FeeStructureID, &i.Title, &i.Amount, &i.Currency, &i.Period,
		&i.DueDate, &i.Status, &i.PaidAmount, &i.PaidAt, &i.Method, &i.Note, &i.CreatedAt, &i.UpdatedAt)
	return i, err
}
