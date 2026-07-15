package staff

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

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

const columns = `id, staff_no, full_name, role, email, phone, user_id, status, joined_on, created_at, updated_at`

func (r *PostgresRepository) Create(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewStaff) (Staff, error) {
	var s Staff
	err := tx.QueryRow(ctx,
		`INSERT INTO staff (tenant_id, staff_no, full_name, role, email, phone, joined_on)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING `+columns,
		tenantID, n.StaffNo, n.FullName, n.Role, n.Email, n.Phone, n.JoinedOn).
		Scan(&s.ID, &s.StaffNo, &s.FullName, &s.Role, &s.Email, &s.Phone, &s.UserID, &s.Status, &s.JoinedOn, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return Staff{}, ErrStaffNoTaken
		}
		return Staff{}, fmt.Errorf("staff: hire: %w", err)
	}
	return s, nil
}

// Two statements, not one with `role = $x OR $x = ”`: each filter shape gets the
// index that covers its filter and its sort, so neither leaves a Sort node.
const rosterAllSQL = `
	SELECT ` + columns + ` FROM staff
	WHERE tenant_id = $1
	  AND ($2::text IS NULL OR (full_name, id) > ($2, $3::uuid))
	ORDER BY full_name, id LIMIT $4`

const rosterByRoleSQL = `
	SELECT ` + columns + ` FROM staff
	WHERE tenant_id = $1 AND role = $5
	  AND ($2::text IS NULL OR (full_name, id) > ($2, $3::uuid))
	ORDER BY full_name, id LIMIT $4`

func (r *PostgresRepository) Roster(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f RosterFilter, after *cursor, limit int) ([]Staff, error) {
	var afterName, afterID any
	if after != nil {
		afterName, afterID = after.Name, after.ID
	}

	var rows pgx.Rows
	var err error
	if f.Role != "" {
		rows, err = tx.Query(ctx, rosterByRoleSQL, tenantID, afterName, afterID, limit, f.Role)
	} else {
		rows, err = tx.Query(ctx, rosterAllSQL, tenantID, afterName, afterID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("staff: roster: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scan)
}

func (r *PostgresRepository) ByID(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Staff, error) {
	rows, err := tx.Query(ctx, `SELECT `+columns+` FROM staff WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return Staff{}, fmt.Errorf("staff: load: %w", err)
	}
	defer rows.Close()
	s, err := pgx.CollectExactlyOneRow(rows, scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return Staff{}, ErrNotFound
	}
	return s, err
}

func (r *PostgresRepository) Update(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, p StaffPatch) (Staff, error) {
	var s Staff
	err := tx.QueryRow(ctx,
		`UPDATE staff SET
		     full_name = coalesce($3, full_name),
		     role      = coalesce($4, role),
		     email     = coalesce($5, email),
		     phone     = coalesce($6, phone),
		     status    = coalesce($7, status),
		     joined_on = coalesce($8::date, joined_on),
		     updated_at = now()
		 WHERE tenant_id = $1 AND id = $2
		 RETURNING `+columns,
		tenantID, id, p.FullName, p.Role, p.Email, p.Phone, p.Status, p.JoinedOn).
		Scan(&s.ID, &s.StaffNo, &s.FullName, &s.Role, &s.Email, &s.Phone, &s.UserID, &s.Status, &s.JoinedOn, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Staff{}, ErrNotFound
	}
	if err != nil {
		return Staff{}, fmt.Errorf("staff: update: %w", err)
	}
	return s, nil
}

func (r *PostgresRepository) Delete(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error {
	tag, err := tx.Exec(ctx, `DELETE FROM staff WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("staff: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scan(row pgx.CollectableRow) (Staff, error) {
	var s Staff
	err := row.Scan(&s.ID, &s.StaffNo, &s.FullName, &s.Role, &s.Email, &s.Phone, &s.UserID, &s.Status, &s.JoinedOn, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}
