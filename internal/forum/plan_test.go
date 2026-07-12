package forum_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/forum"
	"github.com/ebnsina/muallim-api/internal/platform/database"
)

func explain(t *testing.T, db *database.DB, tenantID uuid.UUID, sql string, args ...any) string {
	t.Helper()
	var plan strings.Builder
	err := db.WithTenantReadOnly(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, COSTS OFF) "+sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				return err
			}
			plan.WriteString(line + "\n")
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	return plan.String()
}

// A space with many threads, so the planner must choose between the thread index
// and a sort. Activity times are scattered, and a tenth are pinned, so the order
// the index encodes — pinned first, then most recent — is actually exercised.
func seedManyThreads(t *testing.T, db *database.DB, tenantID, spaceID uuid.UUID, n int) {
	t.Helper()
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO forum_threads
				(tenant_id, space_id, title, body, pinned, last_activity_at, created_at)
			SELECT $1, $2, 'T' || g, 'body', (g % 10 = 0),
			       now() - ((g * 7 % 5000) || ' minutes')::interval,
			       now() - (g || ' minutes')::interval
			FROM generate_series(1, $3) g`, tenantID, spaceID, n)
		return err
	})
	if err != nil {
		t.Fatalf("seed threads: %v", err)
	}
	if err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `ANALYZE forum_threads`)
		return err
	}); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

// The thread list must read forum_threads_space_idx in order — pinned first, then
// most recently active — never a Sort. The three-part order is the trap: the index
// ends in `id DESC`, so the query must too, or the planner sorts.
func TestThreadListIsAnIndexScanWithoutASortNode(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db, nil)
	tenantID := seedTenant(t, db)
	ws, err := svc.CreateSpace(t.Context(), tenantID, "", "Commons", "")
	if err != nil {
		t.Fatalf("space: %v", err)
	}
	seedManyThreads(t, db, tenantID, ws.ID, 4000)

	// The first page: null cursor, so the keyset predicate is inert.
	plan := explain(t, db, tenantID, forum.ThreadsSQL, tenantID, ws.ID, nil, nil, nil, 21)

	if strings.Contains(plan, "Seq Scan on forum_threads") {
		t.Errorf("thread list reads the whole table:\n%s", plan)
	}
	if strings.Contains(plan, "Sort") {
		t.Errorf("thread list sorts rather than reading the ordered index:\n%s", plan)
	}
	if !strings.Contains(plan, "forum_threads_space_idx") {
		t.Errorf("thread list does not use its index:\n%s", plan)
	}
}
