package payroll

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

const salaryColumns = `id, staff_id, basic_amount, allowances_amount, deductions_amount,
	currency, effective_from, created_at, updated_at`

// UpsertSalary sets a staff member's salary, replacing the current one in place.
func (r *PostgresRepository) UpsertSalary(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewSalary) (SalaryStructure, error) {
	rows, err := tx.Query(ctx,
		`INSERT INTO payroll_salary_structures
		     (tenant_id, staff_id, basic_amount, allowances_amount, deductions_amount, currency, effective_from)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (tenant_id, staff_id) DO UPDATE SET
		     basic_amount = EXCLUDED.basic_amount,
		     allowances_amount = EXCLUDED.allowances_amount,
		     deductions_amount = EXCLUDED.deductions_amount,
		     currency = EXCLUDED.currency,
		     effective_from = EXCLUDED.effective_from,
		     updated_at = now()
		 RETURNING `+salaryColumns,
		tenantID, n.StaffID, n.BasicAmount, n.AllowancesAmount, n.DeductionsAmount, n.Currency, n.EffectiveFrom)
	if err != nil {
		return SalaryStructure{}, fmt.Errorf("payroll: upsert salary: %w", err)
	}
	st, err := pgx.CollectExactlyOneRow(rows, scanSalary)
	if isForeignKeyViolation(err) {
		return SalaryStructure{}, ErrNotFound
	}
	if err != nil {
		return SalaryStructure{}, fmt.Errorf("payroll: upsert salary: %w", err)
	}
	return st, nil
}

func (r *PostgresRepository) SalaryByStaff(ctx context.Context, tx pgx.Tx, tenantID, staffID uuid.UUID) (SalaryStructure, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+salaryColumns+` FROM payroll_salary_structures
		 WHERE tenant_id = $1 AND staff_id = $2`, tenantID, staffID)
	if err != nil {
		return SalaryStructure{}, fmt.Errorf("payroll: load salary: %w", err)
	}
	st, err := pgx.CollectExactlyOneRow(rows, scanSalary)
	if errors.Is(err, pgx.ErrNoRows) {
		return SalaryStructure{}, ErrNotFound
	}
	return st, err
}

// GeneratePayslips draws one payslip per staff member with a salary structure for a
// period, skipping any already generated for it. One statement: the gross, net and
// deduction are computed from the structure in the same INSERT ... SELECT.
func (r *PostgresRepository) GeneratePayslips(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, b Batch) (int, error) {
	tag, err := tx.Exec(ctx,
		`INSERT INTO payroll_payslips
		     (tenant_id, staff_id, period, gross_amount, deductions_amount, net_amount, currency)
		 SELECT s.tenant_id, s.staff_id, $2,
		        s.basic_amount + s.allowances_amount,
		        s.deductions_amount,
		        s.basic_amount + s.allowances_amount - s.deductions_amount,
		        s.currency
		 FROM payroll_salary_structures s
		 WHERE s.tenant_id = $1 AND ($3::uuid IS NULL OR s.staff_id = $3)
		 ON CONFLICT (tenant_id, staff_id, period) DO NOTHING`,
		tenantID, b.Period, b.StaffID)
	if err != nil {
		return 0, fmt.Errorf("payroll: generate payslips: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

const payslipColumns = `id, staff_id, period, gross_amount, deductions_amount, net_amount,
	currency, status, generated_at, paid_at, method, created_at, updated_at`

// Two statements, not one with an `OR $x IS NULL` predicate: the by-staff shape gets
// the index that covers it, and neither leaves a Sort node on the request path. The
// period and status filters ride the same index-ordered scan.
const payslipsAllSQL = `
	SELECT ` + payslipColumns + ` FROM payroll_payslips
	WHERE tenant_id = $1
	  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4::uuid))
	  AND ($5::text = '' OR period = $5)
	  AND ($6::text = '' OR status = $6)
	ORDER BY created_at DESC, id DESC LIMIT $2`

const payslipsByStaffSQL = `
	SELECT ` + payslipColumns + ` FROM payroll_payslips
	WHERE tenant_id = $1 AND staff_id = $7
	  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4::uuid))
	  AND ($5::text = '' OR period = $5)
	  AND ($6::text = '' OR status = $6)
	ORDER BY created_at DESC, id DESC LIMIT $2`

func (r *PostgresRepository) Payslips(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f PayslipFilter, after *cursor, limit int) ([]Payslip, error) {
	var afterTime, afterID any
	if after != nil {
		afterTime, afterID = after.CreatedAt, after.ID
	}

	var rows pgx.Rows
	var err error
	if f.StaffID != nil {
		rows, err = tx.Query(ctx, payslipsByStaffSQL, tenantID, limit, afterTime, afterID, f.Period, f.Status, *f.StaffID)
	} else {
		rows, err = tx.Query(ctx, payslipsAllSQL, tenantID, limit, afterTime, afterID, f.Period, f.Status)
	}
	if err != nil {
		return nil, fmt.Errorf("payroll: payslips: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanPayslip)
}

func (r *PostgresRepository) MarkPaid(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, method string) (Payslip, error) {
	rows, err := tx.Query(ctx,
		`UPDATE payroll_payslips
		 SET status = 'paid', paid_at = now(), method = $3, updated_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND status = 'draft'
		 RETURNING `+payslipColumns,
		tenantID, id, method)
	if err != nil {
		return Payslip{}, fmt.Errorf("payroll: mark paid: %w", err)
	}
	ps, err := pgx.CollectExactlyOneRow(rows, scanPayslip)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payslip{}, r.absentOrPaid(ctx, tx, tenantID, id)
	}
	return ps, err
}

// absentOrPaid distinguishes a payslip that does not exist from one that is no
// longer a draft, so the caller can 404 the first and 409 the second.
func (r *PostgresRepository) absentOrPaid(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT true FROM payroll_payslips WHERE tenant_id = $1 AND id = $2`, tenantID, id).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("payroll: check payslip: %w", err)
	}
	return ErrNotDraft
}

func scanSalary(row pgx.CollectableRow) (SalaryStructure, error) {
	var s SalaryStructure
	err := row.Scan(&s.ID, &s.StaffID, &s.BasicAmount, &s.AllowancesAmount, &s.DeductionsAmount,
		&s.Currency, &s.EffectiveFrom, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

func scanPayslip(row pgx.CollectableRow) (Payslip, error) {
	var p Payslip
	err := row.Scan(&p.ID, &p.StaffID, &p.Period, &p.GrossAmount, &p.DeductionsAmount, &p.NetAmount,
		&p.Currency, &p.Status, &p.GeneratedAt, &p.PaidAt, &p.Method, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}
