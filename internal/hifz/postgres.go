package hifz

import (
	"context"
	"errors"
	"fmt"
	"time"

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

const columns = `id, student_id, on_date, kind, surah, ayah_from, ayah_to, rating, note, created_at`

func (r *PostgresRepository) Create(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewEntry, recordedBy uuid.UUID) (Entry, error) {
	rows, err := tx.Query(ctx,
		`INSERT INTO hifz_entries
		     (tenant_id, student_id, on_date, kind, surah, ayah_from, ayah_to, rating, note, recorded_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 RETURNING `+columns,
		tenantID, n.StudentID, n.OnDate, n.Kind, n.Surah, n.AyahFrom, n.AyahTo, n.Rating, n.Note, recordedBy)
	if err != nil {
		return Entry{}, fmt.Errorf("hifz: log: %w", err)
	}
	e, err := pgx.CollectExactlyOneRow(rows, scan)
	if isForeignKeyViolation(err) {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, fmt.Errorf("hifz: log: %w", err)
	}
	return e, nil
}

func (r *PostgresRepository) StudentLog(ctx context.Context, tx pgx.Tx, tenantID, studentID uuid.UUID, after *cursor, limit int) ([]Entry, error) {
	var afterDate, afterID any
	if after != nil {
		afterDate, afterID = after.OnDate, after.ID
	}
	rows, err := tx.Query(ctx,
		`SELECT `+columns+` FROM hifz_entries
		 WHERE tenant_id = $1 AND student_id = $2
		   AND ($3::date IS NULL OR (on_date, id) < ($3, $4::uuid))
		 ORDER BY on_date DESC, id DESC LIMIT $5`,
		tenantID, studentID, afterDate, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("hifz: student log: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scan)
}

func (r *PostgresRepository) LatestSabaq(ctx context.Context, tx pgx.Tx, tenantID, studentID uuid.UUID) (Entry, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+columns+` FROM hifz_entries
		 WHERE tenant_id = $1 AND student_id = $2 AND kind = 'sabaq'
		 ORDER BY on_date DESC, id DESC LIMIT 1`,
		tenantID, studentID)
	if err != nil {
		return Entry{}, fmt.Errorf("hifz: latest sabaq: %w", err)
	}
	e, err := pgx.CollectExactlyOneRow(rows, scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, ErrNotFound
	}
	return e, err
}

func (r *PostgresRepository) CountsByKind(ctx context.Context, tx pgx.Tx, tenantID, studentID uuid.UUID, since time.Time) (map[string]int, error) {
	rows, err := tx.Query(ctx,
		`SELECT kind, count(*) FROM hifz_entries
		 WHERE tenant_id = $1 AND student_id = $2 AND on_date >= $3::date
		 GROUP BY kind`,
		tenantID, studentID, since)
	if err != nil {
		return nil, fmt.Errorf("hifz: counts: %w", err)
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, err
		}
		counts[kind] = n
	}
	return counts, rows.Err()
}

func (r *PostgresRepository) Delete(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error {
	tag, err := tx.Exec(ctx, `DELETE FROM hifz_entries WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("hifz: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scan(row pgx.CollectableRow) (Entry, error) {
	var e Entry
	err := row.Scan(&e.ID, &e.StudentID, &e.OnDate, &e.Kind, &e.Surah, &e.AyahFrom, &e.AyahTo, &e.Rating, &e.Note, &e.CreatedAt)
	return e, err
}
