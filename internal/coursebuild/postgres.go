package coursebuild

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

const columns = `id, name, description, structure, created_at, updated_at`

func (r *PostgresRepository) Create(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewBlueprint) (Blueprint, error) {
	rows, err := tx.Query(ctx,
		`INSERT INTO course_blueprints (tenant_id, name, description, structure)
		 VALUES ($1, $2, $3, $4)
		 RETURNING `+columns,
		tenantID, n.Name, n.Description, []byte(n.Structure))
	if err != nil {
		return Blueprint{}, fmt.Errorf("coursebuild: create: %w", err)
	}
	b, err := pgx.CollectExactlyOneRow(rows, scanBlueprint)
	if err != nil {
		return Blueprint{}, fmt.Errorf("coursebuild: create: %w", err)
	}
	return b, nil
}

const listSQL = `
	SELECT ` + columns + ` FROM course_blueprints
	WHERE tenant_id = $1
	  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4::uuid))
	ORDER BY created_at DESC, id DESC LIMIT $2`

func (r *PostgresRepository) List(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *cursor, limit int) ([]Blueprint, error) {
	var afterTime, afterID any
	if after != nil {
		afterTime, afterID = after.CreatedAt, after.ID
	}
	rows, err := tx.Query(ctx, listSQL, tenantID, limit, afterTime, afterID)
	if err != nil {
		return nil, fmt.Errorf("coursebuild: list: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanBlueprint)
}

func (r *PostgresRepository) Get(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Blueprint, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+columns+` FROM course_blueprints WHERE tenant_id = $1 AND id = $2`,
		tenantID, id)
	if err != nil {
		return Blueprint{}, fmt.Errorf("coursebuild: get: %w", err)
	}
	b, err := pgx.CollectExactlyOneRow(rows, scanBlueprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return Blueprint{}, ErrNotFound
	}
	if err != nil {
		return Blueprint{}, fmt.Errorf("coursebuild: get: %w", err)
	}
	return b, nil
}

// Update patches only the fields the caller set. COALESCE keeps a nil field as it
// stands, so a name-only edit never disturbs the structure and vice versa.
func (r *PostgresRepository) Update(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, p BlueprintPatch) (Blueprint, error) {
	var structure []byte
	if p.Structure != nil {
		structure = []byte(p.Structure)
	}
	rows, err := tx.Query(ctx,
		`UPDATE course_blueprints
		 SET name        = COALESCE($3::text, name),
		     description = COALESCE($4::text, description),
		     structure   = COALESCE($5::jsonb, structure),
		     updated_at  = now()
		 WHERE tenant_id = $1 AND id = $2
		 RETURNING `+columns,
		tenantID, id, p.Name, p.Description, structure)
	if err != nil {
		return Blueprint{}, fmt.Errorf("coursebuild: update: %w", err)
	}
	b, err := pgx.CollectExactlyOneRow(rows, scanBlueprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return Blueprint{}, ErrNotFound
	}
	if err != nil {
		return Blueprint{}, fmt.Errorf("coursebuild: update: %w", err)
	}
	return b, nil
}

func (r *PostgresRepository) Delete(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error {
	tag, err := tx.Exec(ctx,
		`DELETE FROM course_blueprints WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("coursebuild: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanBlueprint(row pgx.CollectableRow) (Blueprint, error) {
	var b Blueprint
	var structure []byte
	err := row.Scan(&b.ID, &b.Name, &b.Description, &structure, &b.CreatedAt, &b.UpdatedAt)
	b.Structure = structure
	return b, err
}
