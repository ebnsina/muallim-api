package calendar

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

// isCheckViolation reports a CHECK constraint failure — an ends_on before its
// starts_on that a patch moved one end into.
func isCheckViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23514"
}

const columns = `id, title, description, kind, starts_on, ends_on, created_at, updated_at`

func (r *PostgresRepository) Create(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewEvent) (Event, error) {
	var e Event
	err := tx.QueryRow(ctx,
		`INSERT INTO calendar_events (tenant_id, title, description, kind, starts_on, ends_on)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING `+columns,
		tenantID, n.Title, n.Description, n.Kind, n.StartsOn, n.EndsOn).
		Scan(&e.ID, &e.Title, &e.Description, &e.Kind, &e.StartsOn, &e.EndsOn, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		if isCheckViolation(err) {
			return Event{}, ErrInvalidEvent
		}
		return Event{}, fmt.Errorf("calendar: create: %w", err)
	}
	return e, nil
}

// One statement covers every filter shape: the keyset rides the
// (tenant_id, starts_on DESC, id DESC) index, and the from/to window rides
// (tenant_id, starts_on) — neither leaves a Sort node on the request path.
const listSQL = `
	SELECT ` + columns + ` FROM calendar_events
	WHERE tenant_id = $1
	  AND ($2::date IS NULL OR starts_on >= $2)
	  AND ($3::date IS NULL OR starts_on <= $3)
	  AND ($4::text IS NULL OR kind = $4)
	  AND ($5::date IS NULL OR (starts_on, id) < ($5, $6::uuid))
	ORDER BY starts_on DESC, id DESC LIMIT $7`

func (r *PostgresRepository) List(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f EventFilter, after *cursor, limit int) ([]Event, error) {
	var afterStarts, afterID any
	if after != nil {
		afterStarts, afterID = after.StartsOn, after.ID
	}
	var kind any
	if f.Kind != "" {
		kind = f.Kind
	}
	var from, to any
	if f.From != nil {
		from = *f.From
	}
	if f.To != nil {
		to = *f.To
	}

	rows, err := tx.Query(ctx, listSQL, tenantID, from, to, kind, afterStarts, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("calendar: list: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scan)
}

func (r *PostgresRepository) Update(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, p EventPatch) (Event, error) {
	var e Event
	err := tx.QueryRow(ctx,
		`UPDATE calendar_events SET
		     title       = coalesce($3, title),
		     description = coalesce($4, description),
		     kind        = coalesce($5, kind),
		     starts_on   = coalesce($6::date, starts_on),
		     ends_on     = coalesce($7::date, ends_on),
		     updated_at  = now()
		 WHERE tenant_id = $1 AND id = $2
		 RETURNING `+columns,
		tenantID, id, p.Title, p.Description, p.Kind, p.StartsOn, p.EndsOn).
		Scan(&e.ID, &e.Title, &e.Description, &e.Kind, &e.StartsOn, &e.EndsOn, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		if isCheckViolation(err) {
			return Event{}, ErrInvalidEvent
		}
		return Event{}, fmt.Errorf("calendar: update: %w", err)
	}
	return e, nil
}

func (r *PostgresRepository) Delete(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error {
	tag, err := tx.Exec(ctx, `DELETE FROM calendar_events WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("calendar: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scan(row pgx.CollectableRow) (Event, error) {
	var e Event
	err := row.Scan(&e.ID, &e.Title, &e.Description, &e.Kind, &e.StartsOn, &e.EndsOn, &e.CreatedAt, &e.UpdatedAt)
	return e, err
}
