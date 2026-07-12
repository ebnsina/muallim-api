package tenant

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// PostgresRepository satisfies Repository.
type PostgresRepository struct{ db *database.DB }

// NewPostgresRepository returns a Repository backed by Postgres.
func NewPostgresRepository(db *database.DB) *PostgresRepository {
	return &PostgresRepository{db: db}
}

// byHostSQL matches a custom domain or a subdomain in one round trip.
//
// Both branches are covered by unique indexes on lower(custom_domain) and
// lower(subdomain), so this is two index lookups, not a scan. A custom domain
// wins over a subdomain: a tenant who has configured one has chosen it.
const byHostSQL = `
	SELECT id, subdomain, coalesce(custom_domain, ''), name, status
	FROM tenants
	WHERE lower(custom_domain) = $1 OR lower(subdomain) = $2
	ORDER BY (lower(custom_domain) = $1) DESC
	LIMIT 1`

// ByHost resolves a normalised host. The tenants table is the isolation boundary
// itself and carries no row-level security policy, so this query runs outside a
// tenant binding — the one place in the codebase that is true.
func (r *PostgresRepository) ByHost(ctx context.Context, host string) (Tenant, error) {
	var t Tenant

	err := r.db.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, byHostSQL, host, subdomainOf(host))
		return row.Scan(&t.ID, &t.Subdomain, &t.CustomDomain, &t.Name, &t.Status)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Tenant{}, ErrNotFound
		}
		return Tenant{}, fmt.Errorf("tenant: resolve host %q: %w", host, err)
	}
	return t, nil
}

// subdomainOf returns the first label of a host. A bare host with no dot is
// itself the label, which is what makes "localhost" resolvable in development.
func subdomainOf(host string) string {
	label, _, found := strings.Cut(host, ".")
	if !found {
		return host
	}
	return label
}
