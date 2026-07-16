// Package database owns the Postgres connection pool and the transaction
// helpers that bind a tenant to row-level security.
//
// It has no domain knowledge. Repositories in domain packages receive a pgx.Tx
// from WithTenant and never see the pool itself.
package database

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Options configures the pool. The defaults are deliberate: a request that is
// still waiting on a connection after a second is already a failed request, and
// a query that has run for five seconds is a bug or an unindexed scan.
type Options struct {
	URL string

	MaxConns int32
	MinConns int32

	// StatementTimeout bounds any single query at the server, so a pathological
	// plan cannot hold a connection open indefinitely. Postgres cancels it and
	// the client sees an error instead of a hang.
	StatementTimeout time.Duration

	// SlowQueryThreshold is the duration above which a query is logged at warn.
	SlowQueryThreshold time.Duration

	ConnectTimeout time.Duration
}

// DB wraps a pgx pool.
type DB struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// New connects and verifies the connection before returning. A DB that exists is
// a DB that works.
func New(ctx context.Context, opts Options, log *slog.Logger) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(opts.URL)
	if err != nil {
		return nil, fmt.Errorf("database: parse url: %w", err)
	}

	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.MinConns > 0 {
		cfg.MinConns = opts.MinConns
	}
	if opts.StatementTimeout > 0 {
		ms := int64(opts.StatementTimeout / time.Millisecond)
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = fmt.Sprintf("%d", ms)
	}

	cfg.ConnConfig.Tracer = &tracer{log: log, slow: opts.SlowQueryThreshold}

	// Recycle connections so a long-lived process cannot accumulate server-side
	// state or pin a stale plan forever.
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cmp(opts.ConnectTimeout, 5*time.Second))
	defer cancel()

	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: ping: %w", err)
	}

	return &DB{pool: pool, log: log}, nil
}

// Ping reports whether Postgres is reachable. Readiness probes use it; liveness
// probes must not.
func (db *DB) Ping(ctx context.Context) error {
	if err := db.pool.Ping(ctx); err != nil {
		return fmt.Errorf("database: ping: %w", err)
	}
	return nil
}

// Close drains the pool.
func (db *DB) Close() { db.pool.Close() }

// Notify sends a Postgres NOTIFY on channel with payload, on the pool
// (autocommit), so it fires at once rather than on some transaction's commit.
// It is the fan-out primitive realtime features (chat) publish through; it
// touches no table, so it is not tenant-bound.
func (db *DB) Notify(ctx context.Context, channel, payload string) error {
	if _, err := db.pool.Exec(ctx, "SELECT pg_notify($1, $2)", channel, payload); err != nil {
		return fmt.Errorf("database: notify %s: %w", channel, err)
	}
	return nil
}

// Listen holds one dedicated connection LISTENing on channel and calls onNotify
// for every notification until ctx is cancelled, reconnecting on a dropped
// connection. This is how a chat event published on one API instance reaches the
// WebSocket connections held by every instance.
func (db *DB) Listen(ctx context.Context, channel string, onNotify func(payload string)) {
	for ctx.Err() == nil {
		if err := db.listenOnce(ctx, channel, onNotify); err != nil && ctx.Err() == nil {
			db.log.Warn("listen dropped; reconnecting", "channel", channel, "error", err)
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}

func (db *DB) listenOnce(ctx context.Context, channel string, onNotify func(string)) error {
	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	// channel is a fixed constant from our own code, never user input.
	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return err
	}
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		onNotify(n.Payload)
	}
}

func cmp(v, fallback time.Duration) time.Duration {
	if v <= 0 {
		return fallback
	}
	return v
}
