package database

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

type ctxKey int

const (
	ctxKeyQueryStart ctxKey = iota
	ctxKeyCounter
	ctxKeySQL
)

// Counter tallies the queries issued under a context. It exists so that N+1
// regressions are caught by an assertion rather than by a customer.
//
// A test wraps a context with WithCounter, exercises a service method, and
// asserts the count. A loop that issues one query per row makes the count grow
// with the fixture, and the test fails.
type Counter struct{ n atomic.Int64 }

// Count returns the number of queries traced so far.
func (c *Counter) Count() int { return int(c.n.Load()) }

// Reset zeroes the tally, so one test can measure several calls.
func (c *Counter) Reset() { c.n.Store(0) }

// WithCounter returns a context whose queries are counted by c.
func WithCounter(ctx context.Context, c *Counter) context.Context {
	return context.WithValue(ctx, ctxKeyCounter, c)
}

// tracer implements pgx.QueryTracer. It counts queries and reports slow ones.
type tracer struct {
	log  *slog.Logger
	slow time.Duration
}

func (t *tracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !isPlumbing(data.SQL) {
		if c, ok := ctx.Value(ctxKeyCounter).(*Counter); ok {
			c.n.Add(1)
		}
	}
	ctx = context.WithValue(ctx, ctxKeySQL, data.SQL)
	return context.WithValue(ctx, ctxKeyQueryStart, time.Now())
}

// isPlumbing reports whether a statement is transaction machinery rather than a
// domain query. pgx traces BEGIN and COMMIT as queries, and every tenant-scoped
// read binds app.tenant_id before doing anything else.
//
// Counting those would make a Counter report a constant overhead of three, so
// every N+1 assertion would read "expected 6" where a reviewer expects 3 — and an
// assertion nobody can read at a glance is an assertion nobody maintains.
func isPlumbing(sql string) bool {
	if sql == bindTenantSQL {
		return true
	}
	switch {
	case hasPrefixFold(sql, "begin"),
		hasPrefixFold(sql, "commit"),
		hasPrefixFold(sql, "rollback"),
		hasPrefixFold(sql, "savepoint"),
		hasPrefixFold(sql, "release"):
		return true
	}
	return false
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func (t *tracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	start, ok := ctx.Value(ctxKeyQueryStart).(time.Time)
	if !ok {
		return
	}
	elapsed := time.Since(start)

	if data.Err != nil {
		// Cancellation is the client hanging up, not a database fault.
		if ctx.Err() != nil {
			return
		}
		t.log.ErrorContext(ctx, "query failed",
			slog.String("error", data.Err.Error()),
			slog.Duration("elapsed", elapsed),
		)
		return
	}

	if t.slow > 0 && elapsed >= t.slow {
		sql, ok := ctx.Value(ctxKeySQL).(string)
		if !ok {
			sql = "unknown"
		}

		// The statement text is safe to log; the arguments are not, and are never
		// included. A slow-query log that omits the statement cannot tell you which
		// query was slow, which is the only question it exists to answer.
		t.log.WarnContext(ctx, "slow query",
			slog.Duration("elapsed", elapsed),
			slog.Int64("rows", data.CommandTag.RowsAffected()),
			slog.String("sql", collapse(sql)),
		)
	}
}

// collapse folds a multi-line statement onto one line so a log record stays one
// record, and truncates it so a pathological generated query cannot flood the log.
func collapse(sql string) string {
	sql = strings.Join(strings.Fields(sql), " ")
	const max = 300
	if len(sql) > max {
		return sql[:max] + "..."
	}
	return sql
}
