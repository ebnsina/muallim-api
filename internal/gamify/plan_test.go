package gamify_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/gamify"
	"github.com/ebnsina/lms-api/internal/platform/database"
)

// seedManyRanked inserts n learners with points, so the planner has a real choice
// between reading the leaderboard index and sorting the table. A sort over a few
// rows looks fine and is a time bomb; the plan is only informative at size.
func seedManyRanked(t *testing.T, db *database.DB, tenantID uuid.UUID, n int) {
	t.Helper()
	// A per-run salt keeps deterministic ids unique across parallel tests, so two
	// never collide on the global users email.
	salt := uuid.NewString()
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// users: $1 salt, $2 n.
		if _, err := tx.Exec(ctx,
			`INSERT INTO users (id, email, password_hash, name)
			 SELECT md5($1 || g::text)::uuid, md5($1 || g::text) || '@x.test', 'x', 'Learner'
			 FROM generate_series(1, $2) g`, salt, n); err != nil {
			return err
		}
		// memberships and points: $1 tenant, $2 salt, $3 n.
		if _, err := tx.Exec(ctx,
			`INSERT INTO memberships (tenant_id, user_id, role)
			 SELECT $1, md5($2 || g::text)::uuid, 'student' FROM generate_series(1, $3) g`,
			tenantID, salt, n); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO user_points (tenant_id, user_id, points)
			 SELECT $1, md5($2 || g::text)::uuid, (g * 7) % 1000 FROM generate_series(1, $3) g`,
			tenantID, salt, n)
		return err
	})
	if err != nil {
		t.Fatalf("seed ranked: %v", err)
	}

	err = db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `ANALYZE user_points`)
		return err
	})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

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
			plan.WriteString(line)
			plan.WriteString("\n")
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	return plan.String()
}

// The leaderboard must be served by the (tenant, points DESC, user_id) index: an
// ordered index scan capped by the LIMIT, never a Sort over every ranked learner.
// A Sort here is the last-page problem — it gets slower as a workspace grows.
func TestLeaderboardIsAnIndexScanWithoutASortNode(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenantID := seedTenant(t, db)
	seedManyRanked(t, db, tenantID, 3000)

	plan := explain(t, db, tenantID, gamify.LeaderboardSQL, tenantID, 20)

	if strings.Contains(plan, "Seq Scan on user_points") {
		t.Errorf("leaderboard reads the whole points table:\n%s", plan)
	}
	if strings.Contains(plan, "Sort") {
		t.Errorf("leaderboard sorts in memory rather than reading the ordered index:\n%s", plan)
	}
	if !strings.Contains(plan, "user_points_leaderboard_idx") {
		t.Errorf("leaderboard does not use its index:\n%s", plan)
	}
}
