package notify_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/notify"
	"github.com/ebnsina/lms-api/internal/platform/database"
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

// Many notifications for one recipient, some sharing an instant — the way a
// fan-out stamps a batch — so the id tie-break is actually exercised.
func seedManyNotifications(t *testing.T, db *database.DB, tenantID, userID uuid.UUID, n int) {
	t.Helper()
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO notifications (tenant_id, user_id, kind, title, created_at)
			SELECT $1, $2, 'answer', 'T' || g, now() - ((g / 5) || ' minutes')::interval
			FROM generate_series(1, $3) g`, tenantID, userID, n)
		return err
	})
	if err != nil {
		t.Fatalf("seed notifications: %v", err)
	}
	if err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `ANALYZE notifications`)
		return err
	}); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

// The bell list must read notifications_recipient_idx in order, never a Sort — not
// even an incremental one over the same-instant groups a fan-out creates.
func TestNotificationListIsAnIndexScanWithoutASortNode(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenantID := seedTenant(t, db)
	me := seedUser(t, db, tenantID)
	seedManyNotifications(t, db, tenantID, me, 5000)

	// First page: null cursor, so the keyset predicate is inert.
	plan := explain(t, db, tenantID, notify.ListSQL, tenantID, me, nil, nil, 21)

	if strings.Contains(plan, "Seq Scan on notifications") {
		t.Errorf("notification list reads the whole table:\n%s", plan)
	}
	if strings.Contains(plan, "Sort") {
		t.Errorf("notification list sorts rather than reading the ordered index:\n%s", plan)
	}
	if !strings.Contains(plan, "notifications_recipient_idx") {
		t.Errorf("notification list does not use its index:\n%s", plan)
	}
}
