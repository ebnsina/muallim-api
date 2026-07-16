package certdesign

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgresRepository is the Postgres-backed Repository.
type PostgresRepository struct{}

// NewPostgresRepository returns a PostgresRepository.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

const designColumns = `id, name, orientation, accent, background_color, background_key, layout, created_at, updated_at`

func (r *PostgresRepository) Create(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewDesign) (Design, error) {
	rows, err := tx.Query(ctx,
		`INSERT INTO certificate_designs (tenant_id, name, orientation, accent, background_color, layout)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING `+designColumns,
		tenantID, n.Name, n.Orientation, n.Accent, n.BackgroundColor, []byte(n.Layout))
	if err != nil {
		return Design{}, fmt.Errorf("certdesign: create: %w", err)
	}
	d, err := pgx.CollectExactlyOneRow(rows, scanDesign)
	if err != nil {
		return Design{}, fmt.Errorf("certdesign: create: %w", err)
	}
	return d, nil
}

const listSQL = `
	SELECT ` + designColumns + ` FROM certificate_designs
	WHERE tenant_id = $1
	  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4::uuid))
	ORDER BY created_at DESC, id DESC LIMIT $2`

func (r *PostgresRepository) List(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *cursor, limit int) ([]Design, error) {
	var afterTime, afterID any
	if after != nil {
		afterTime, afterID = after.CreatedAt, after.ID
	}
	rows, err := tx.Query(ctx, listSQL, tenantID, limit, afterTime, afterID)
	if err != nil {
		return nil, fmt.Errorf("certdesign: list: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanDesign)
}

func (r *PostgresRepository) ByID(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Design, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+designColumns+` FROM certificate_designs WHERE tenant_id = $1 AND id = $2`,
		tenantID, id)
	if err != nil {
		return Design{}, fmt.Errorf("certdesign: by id: %w", err)
	}
	d, err := pgx.CollectExactlyOneRow(rows, scanDesign)
	if errors.Is(err, pgx.ErrNoRows) {
		return Design{}, ErrNotFound
	}
	if err != nil {
		return Design{}, fmt.Errorf("certdesign: by id: %w", err)
	}
	return d, nil
}

// Update applies a patch. Each column is written only when its argument is non-null,
// so a partial update leaves the rest untouched, in one statement.
func (r *PostgresRepository) Update(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, p DesignPatch) (Design, error) {
	var layout []byte
	if p.Layout != nil {
		layout = []byte(p.Layout)
	}
	rows, err := tx.Query(ctx,
		`UPDATE certificate_designs SET
		   name             = COALESCE($3, name),
		   orientation      = COALESCE($4, orientation),
		   accent           = COALESCE($5, accent),
		   background_color = COALESCE($6, background_color),
		   layout           = COALESCE($7, layout),
		   updated_at       = now()
		 WHERE tenant_id = $1 AND id = $2
		 RETURNING `+designColumns,
		tenantID, id, p.Name, p.Orientation, p.Accent, p.BackgroundColor, layout)
	if err != nil {
		return Design{}, fmt.Errorf("certdesign: update: %w", err)
	}
	d, err := pgx.CollectExactlyOneRow(rows, scanDesign)
	if errors.Is(err, pgx.ErrNoRows) {
		return Design{}, ErrNotFound
	}
	if err != nil {
		return Design{}, fmt.Errorf("certdesign: update: %w", err)
	}
	return d, nil
}

// SetBackground records a background key and returns the key it replaced, so the
// caller can delete the now-unreferenced object.
func (r *PostgresRepository) SetBackground(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, key string) (string, error) {
	var replaced *string
	err := tx.QueryRow(ctx,
		`WITH prev AS (SELECT background_key FROM certificate_designs WHERE tenant_id = $1 AND id = $2)
		 UPDATE certificate_designs SET background_key = $3, updated_at = now()
		 WHERE tenant_id = $1 AND id = $2
		 RETURNING (SELECT background_key FROM prev)`,
		tenantID, id, key).Scan(&replaced)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("certdesign: set background: %w", err)
	}
	if replaced == nil {
		return "", nil
	}
	return *replaced, nil
}

func (r *PostgresRepository) Delete(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error {
	tag, err := tx.Exec(ctx, `DELETE FROM certificate_designs WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("certdesign: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanDesign(row pgx.CollectableRow) (Design, error) {
	var d Design
	var name, orientation, accent, bgColor string
	var bgKey *string
	var layout []byte
	err := row.Scan(&d.ID, &name, &orientation, &accent, &bgColor, &bgKey, &layout, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return Design{}, err
	}
	d.Name, d.Orientation, d.Accent, d.BackgroundColor = name, orientation, accent, bgColor
	if bgKey != nil {
		d.BackgroundKey = *bgKey
	}
	d.Layout = layout
	return d, nil
}
