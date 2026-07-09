package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrNoTenant is returned when a tenant-scoped transaction is requested without
// a tenant. It is a programming error, never a user error.
var ErrNoTenant = errors.New("database: tenant id is required")

// bindTenantSQL binds the tenant for the current transaction. The query tracer
// excludes it from its tally so that a Counter reports domain queries only, and
// an N+1 assertion reads as the number of round trips a reviewer would expect.
const bindTenantSQL = `SELECT set_config('app.tenant_id', $1, true)`

// TxFunc runs inside a transaction bound to one tenant.
type TxFunc func(ctx context.Context, tx pgx.Tx) error

// WithTenant runs fn inside a transaction whose app.tenant_id is bound to
// tenantID, which is what every row-level security policy reads.
//
// The binding is transaction-local (set_config's third argument), so it cannot
// leak onto the next request that borrows the same pooled connection. This is
// the entire reason tenant-scoped work goes through a transaction even when it
// only reads: a session-level SET on a pooled connection would be a cross-tenant
// data leak waiting for the right interleaving.
//
// Repositories receive the pgx.Tx and never touch the pool, so there is no way
// to issue a query that skips this binding.
func (db *DB) WithTenant(ctx context.Context, tenantID uuid.UUID, fn TxFunc) error {
	return db.withTenant(ctx, tenantID, pgx.TxOptions{}, fn)
}

// WithTenantReadOnly is WithTenant for queries that only read. Postgres refuses
// any write inside the transaction, which turns "this endpoint accidentally
// mutates data" from a code review question into a database error.
func (db *DB) WithTenantReadOnly(ctx context.Context, tenantID uuid.UUID, fn TxFunc) error {
	return db.withTenant(ctx, tenantID, pgx.TxOptions{AccessMode: pgx.ReadOnly}, fn)
}

func (db *DB) withTenant(ctx context.Context, tenantID uuid.UUID, opts pgx.TxOptions, fn TxFunc) error {
	if tenantID == uuid.Nil {
		return ErrNoTenant
	}

	tx, err := db.pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("database: begin: %w", err)
	}
	// Rollback after a successful Commit is a no-op, so this is safe as the sole
	// cleanup path and covers every early return and panic.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// set_config, not SET LOCAL: the latter cannot take a bound parameter, and
	// interpolating a value into DDL-shaped SQL is how injection happens. The
	// third argument scopes the setting to this transaction.
	if _, err := tx.Exec(ctx, bindTenantSQL, tenantID.String()); err != nil {
		return fmt.Errorf("database: bind tenant: %w", err)
	}

	if err := fn(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit: %w", err)
	}
	return nil
}

// WithoutTenant runs fn against tables that are not tenant-scoped — `tenants`
// itself, and nothing else. Every call site is a place to look twice.
func (db *DB) WithoutTenant(ctx context.Context, fn TxFunc) error {
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("database: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit: %w", err)
	}
	return nil
}
