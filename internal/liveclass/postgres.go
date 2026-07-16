package liveclass

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

const sessionColumns = `id, course_id, title, description, join_url, starts_at, ends_at,
	host_user_id, created_at, updated_at`

func (r *PostgresRepository) Create(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, n NewSession) (Session, error) {
	var joinURL *string
	if n.JoinURL != "" {
		joinURL = &n.JoinURL
	}
	rows, err := tx.Query(ctx,
		`INSERT INTO live_sessions (tenant_id, course_id, title, description, join_url, starts_at, ends_at, host_user_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING `+sessionColumns,
		tenantID, courseID, n.Title, n.Description, joinURL, n.StartsAt, n.EndsAt, n.HostUserID)
	if err != nil {
		return Session{}, fmt.Errorf("liveclass: create: %w", err)
	}
	sess, err := pgx.CollectExactlyOneRow(rows, scanSession)
	if isForeignKeyViolation(err) {
		// The course does not exist in this tenant.
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("liveclass: create: %w", err)
	}
	return sess, nil
}

const forCourseSQL = `
	SELECT ` + sessionColumns + ` FROM live_sessions
	WHERE tenant_id = $1 AND course_id = $2
	  AND ($4::timestamptz IS NULL OR (starts_at, id) < ($4, $5::uuid))
	ORDER BY starts_at DESC, id DESC LIMIT $3`

func (r *PostgresRepository) ForCourse(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, after *cursor, limit int) ([]Session, error) {
	var afterTime, afterID any
	if after != nil {
		afterTime, afterID = after.StartsAt, after.ID
	}
	rows, err := tx.Query(ctx, forCourseSQL, tenantID, courseID, limit, afterTime, afterID)
	if err != nil {
		return nil, fmt.Errorf("liveclass: for course: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanSession)
}

func (r *PostgresRepository) ByID(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Session, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+sessionColumns+` FROM live_sessions WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return Session{}, fmt.Errorf("liveclass: by id: %w", err)
	}
	sess, err := pgx.CollectExactlyOneRow(rows, scanSession)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return sess, err
}

func (r *PostgresRepository) Update(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, s Session) (Session, error) {
	var joinURL *string
	if s.JoinURL != "" {
		joinURL = &s.JoinURL
	}
	rows, err := tx.Query(ctx,
		`UPDATE live_sessions
		 SET title = $3, description = $4, join_url = $5, starts_at = $6, ends_at = $7, updated_at = now()
		 WHERE tenant_id = $1 AND id = $2
		 RETURNING `+sessionColumns,
		tenantID, s.ID, s.Title, s.Description, joinURL, s.StartsAt, s.EndsAt)
	if err != nil {
		return Session{}, fmt.Errorf("liveclass: update: %w", err)
	}
	sess, err := pgx.CollectExactlyOneRow(rows, scanSession)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return sess, err
}

func (r *PostgresRepository) Delete(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error {
	tag, err := tx.Exec(ctx, `DELETE FROM live_sessions WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("liveclass: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanSession(row pgx.CollectableRow) (Session, error) {
	var s Session
	var joinURL *string
	err := row.Scan(&s.ID, &s.CourseID, &s.Title, &s.Description, &joinURL, &s.StartsAt, &s.EndsAt,
		&s.HostUserID, &s.CreatedAt, &s.UpdatedAt)
	if joinURL != nil {
		s.JoinURL = *joinURL
	}
	return s, err
}
