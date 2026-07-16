package admissions

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

const columns = `id, applicant_name, guardian_name, guardian_phone, guardian_email,
	grade_level_id, dob, status, note, student_id, submitted_at, decided_at, created_at, updated_at`

func (r *PostgresRepository) Create(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewApplication) (Application, error) {
	rows, err := tx.Query(ctx,
		`INSERT INTO admission_applications
		     (tenant_id, applicant_name, guardian_name, guardian_phone, guardian_email, grade_level_id, dob, note)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING `+columns,
		tenantID, n.ApplicantName, n.GuardianName, n.GuardianPhone, n.GuardianEmail, n.GradeLevelID, n.DOB, n.Note)
	if err != nil {
		return Application{}, fmt.Errorf("admissions: create: %w", err)
	}
	app, err := pgx.CollectExactlyOneRow(rows, scan)
	if isForeignKeyViolation(err) {
		return Application{}, ErrNotFound
	}
	if err != nil {
		return Application{}, fmt.Errorf("admissions: create: %w", err)
	}
	return app, nil
}

// Two statements, not one with `status = $x OR $x = ”`: each filter shape gets the
// index that covers its filter and its sort, so neither leaves a Sort node.
const listAllSQL = `
	SELECT ` + columns + ` FROM admission_applications
	WHERE tenant_id = $1
	  AND ($2::timestamptz IS NULL OR (submitted_at, id) < ($2, $3::uuid))
	ORDER BY submitted_at DESC, id DESC LIMIT $4`

const listByStatusSQL = `
	SELECT ` + columns + ` FROM admission_applications
	WHERE tenant_id = $1 AND status = $5
	  AND ($2::timestamptz IS NULL OR (submitted_at, id) < ($2, $3::uuid))
	ORDER BY submitted_at DESC, id DESC LIMIT $4`

func (r *PostgresRepository) List(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f Filter, after *cursor, limit int) ([]Application, error) {
	var afterTime, afterID any
	if after != nil {
		afterTime, afterID = after.SubmittedAt, after.ID
	}

	var rows pgx.Rows
	var err error
	if f.Status != "" {
		rows, err = tx.Query(ctx, listByStatusSQL, tenantID, afterTime, afterID, limit, f.Status)
	} else {
		rows, err = tx.Query(ctx, listAllSQL, tenantID, afterTime, afterID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("admissions: list: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scan)
}

func (r *PostgresRepository) ByID(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Application, error) {
	rows, err := tx.Query(ctx, `SELECT `+columns+` FROM admission_applications WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return Application{}, fmt.Errorf("admissions: load: %w", err)
	}
	defer rows.Close()
	app, err := pgx.CollectExactlyOneRow(rows, scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return Application{}, ErrNotFound
	}
	return app, err
}

// Decide transitions an application from one status to another, stamping decided_at.
// The `status = $3` guard is the whole of the safety: a decision on an application no
// longer in `from` finds no row, which we resolve to absent-or-not-pending.
func (r *PostgresRepository) Decide(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, from, to string) (Application, error) {
	rows, err := tx.Query(ctx,
		`UPDATE admission_applications SET status = $4, decided_at = now(), updated_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND status = $3
		 RETURNING `+columns,
		tenantID, id, from, to)
	if err != nil {
		return Application{}, fmt.Errorf("admissions: decide: %w", err)
	}
	app, err := pgx.CollectExactlyOneRow(rows, scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return Application{}, r.absentOrLocked(ctx, tx, tenantID, id)
	}
	return app, err
}

// Admit closes an accepted application, recording the student it produced. The
// `status = 'accepted'` guard means only an accepted application can be admitted, and
// admitting one twice finds nothing.
func (r *PostgresRepository) Admit(ctx context.Context, tx pgx.Tx, tenantID, id, studentID uuid.UUID) (Application, error) {
	rows, err := tx.Query(ctx,
		`UPDATE admission_applications
		 SET status = 'admitted', student_id = $3, decided_at = now(), updated_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND status = 'accepted'
		 RETURNING `+columns,
		tenantID, id, studentID)
	if err != nil {
		if isForeignKeyViolation(err) {
			return Application{}, ErrNotFound
		}
		return Application{}, fmt.Errorf("admissions: admit: %w", err)
	}
	app, err := pgx.CollectExactlyOneRow(rows, scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return Application{}, r.absentOrLocked(ctx, tx, tenantID, id)
	}
	return app, err
}

// absentOrLocked distinguishes an application that does not exist from one that is no
// longer in the status the caller required, so the caller can 404 the first and 409
// the second.
func (r *PostgresRepository) absentOrLocked(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT true FROM admission_applications WHERE tenant_id = $1 AND id = $2`, tenantID, id).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("admissions: check application: %w", err)
	}
	return ErrNotPending
}

func scan(row pgx.CollectableRow) (Application, error) {
	var a Application
	err := row.Scan(&a.ID, &a.ApplicantName, &a.GuardianName, &a.GuardianPhone, &a.GuardianEmail,
		&a.GradeLevelID, &a.DOB, &a.Status, &a.Note, &a.StudentID, &a.SubmittedAt, &a.DecidedAt, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}
