package idcard

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

const templateColumns = `id, name, subject, orientation, accent, background_color, background_key, layout, created_at, updated_at`

func (r *PostgresRepository) Create(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewTemplate) (Template, error) {
	rows, err := tx.Query(ctx,
		`INSERT INTO id_card_templates (tenant_id, name, subject, orientation, accent, background_color, layout)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING `+templateColumns,
		tenantID, n.Name, n.Subject, n.Orientation, n.Accent, n.BackgroundColor, []byte(n.Layout))
	if err != nil {
		return Template{}, fmt.Errorf("idcard: create: %w", err)
	}
	t, err := pgx.CollectExactlyOneRow(rows, scanTemplate)
	if err != nil {
		return Template{}, fmt.Errorf("idcard: create: %w", err)
	}
	return t, nil
}

const listSQL = `
	SELECT ` + templateColumns + ` FROM id_card_templates
	WHERE tenant_id = $1
	  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4::uuid))
	ORDER BY created_at DESC, id DESC LIMIT $2`

func (r *PostgresRepository) List(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *cursor, limit int) ([]Template, error) {
	var afterTime, afterID any
	if after != nil {
		afterTime, afterID = after.CreatedAt, after.ID
	}
	rows, err := tx.Query(ctx, listSQL, tenantID, limit, afterTime, afterID)
	if err != nil {
		return nil, fmt.Errorf("idcard: list: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanTemplate)
}

func (r *PostgresRepository) ByID(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Template, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+templateColumns+` FROM id_card_templates WHERE tenant_id = $1 AND id = $2`,
		tenantID, id)
	if err != nil {
		return Template{}, fmt.Errorf("idcard: by id: %w", err)
	}
	t, err := pgx.CollectExactlyOneRow(rows, scanTemplate)
	if errors.Is(err, pgx.ErrNoRows) {
		return Template{}, ErrNotFound
	}
	if err != nil {
		return Template{}, fmt.Errorf("idcard: by id: %w", err)
	}
	return t, nil
}

// Update applies a patch. Each column is written only when its argument is
// non-null, so a partial update leaves the rest untouched, in one statement.
func (r *PostgresRepository) Update(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, p TemplatePatch) (Template, error) {
	var layout []byte
	if p.Layout != nil {
		layout = []byte(p.Layout)
	}
	rows, err := tx.Query(ctx,
		`UPDATE id_card_templates SET
		   name             = COALESCE($3, name),
		   subject          = COALESCE($4, subject),
		   orientation      = COALESCE($5, orientation),
		   accent           = COALESCE($6, accent),
		   background_color = COALESCE($7, background_color),
		   layout           = COALESCE($8, layout),
		   updated_at       = now()
		 WHERE tenant_id = $1 AND id = $2
		 RETURNING `+templateColumns,
		tenantID, id, p.Name, p.Subject, p.Orientation, p.Accent, p.BackgroundColor, layout)
	if err != nil {
		return Template{}, fmt.Errorf("idcard: update: %w", err)
	}
	t, err := pgx.CollectExactlyOneRow(rows, scanTemplate)
	if errors.Is(err, pgx.ErrNoRows) {
		return Template{}, ErrNotFound
	}
	if err != nil {
		return Template{}, fmt.Errorf("idcard: update: %w", err)
	}
	return t, nil
}

// SetBackground records a background key and returns the key it replaced, so the
// caller can delete the now-unreferenced object.
func (r *PostgresRepository) SetBackground(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, key string) (string, error) {
	var replaced *string
	err := tx.QueryRow(ctx,
		`WITH prev AS (SELECT background_key FROM id_card_templates WHERE tenant_id = $1 AND id = $2)
		 UPDATE id_card_templates SET background_key = $3, updated_at = now()
		 WHERE tenant_id = $1 AND id = $2
		 RETURNING (SELECT background_key FROM prev)`,
		tenantID, id, key).Scan(&replaced)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("idcard: set background: %w", err)
	}
	if replaced == nil {
		return "", nil
	}
	return *replaced, nil
}

func (r *PostgresRepository) Delete(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error {
	tag, err := tx.Exec(ctx, `DELETE FROM id_card_templates WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("idcard: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanTemplate(row pgx.CollectableRow) (Template, error) {
	var t Template
	var name, subject, orientation, accent, bgColor string
	var bgKey *string
	var layout []byte
	err := row.Scan(&t.ID, &name, &subject, &orientation, &accent, &bgColor, &bgKey, &layout, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return Template{}, err
	}
	t.Name, t.Subject, t.Orientation, t.Accent, t.BackgroundColor = name, subject, orientation, accent, bgColor
	if bgKey != nil {
		t.BackgroundKey = *bgKey
	}
	t.Layout = layout
	return t, nil
}
