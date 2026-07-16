package leads

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PostgresRepository satisfies Repository.
type PostgresRepository struct{}

// NewPostgresRepository returns a Repository backed by Postgres.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

const createSQL = `
	INSERT INTO demo_requests (id, intent, name, email, phone, agreed_at, created_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7)
	RETURNING id, intent, name, email, phone, agreed_at, created_at`

// Create writes the request. The transaction comes from database.WithoutTenant:
// the table has no tenant column and no policy, because the person asking has no
// workspace yet.
func (r *PostgresRepository) Create(ctx context.Context, tx pgx.Tx, d DemoRequest) (DemoRequest, error) {
	var out DemoRequest
	row := tx.QueryRow(ctx, createSQL, d.ID, d.Intent, d.Name, d.Email, d.Phone, d.AgreedAt, d.CreatedAt)
	if err := row.Scan(&out.ID, &out.Intent, &out.Name, &out.Email, &out.Phone,
		&out.AgreedAt, &out.CreatedAt); err != nil {
		return DemoRequest{}, fmt.Errorf("leads: create demo request: %w", err)
	}
	return out, nil
}
