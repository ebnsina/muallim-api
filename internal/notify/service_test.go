package notify_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/notify"
	"github.com/ebnsina/lms-api/internal/platform/database"
)

func testDB(t *testing.T) *database.DB {
	t.Helper()
	url := os.Getenv("LMS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LMS_TEST_DATABASE_URL is not set; skipping database tests")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	db, err := database.New(t.Context(), database.Options{URL: url, MaxConns: 4}, log)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func seedTenant(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, subdomain, name) VALUES ($1, $2, 'Test')`,
			id, "t"+id.String()[:8])
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

func seedUser(t *testing.T, db *database.DB, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO users (id, email, password_hash, name) VALUES ($1, $2, 'x', 'U')`,
			id, "u-"+id.String()[:8]+"@example.test")
		return err
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func newService(db *database.DB) *notify.Service {
	return notify.NewService(db, notify.NewPostgresRepository())
}

// record writes a notification through the service's producer entry point, in a
// throwaway transaction — the way a real producer would, inside its own event.
func record(t *testing.T, db *database.DB, svc *notify.Service, tenantID, userID uuid.UUID, title string) {
	t.Helper()
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return svc.Record(ctx, tx, tenantID, notify.Notification{
			UserID: userID, Kind: notify.KindAnswer, Title: title, Link: "/x",
		})
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
}

func TestRecordListCountAndMarkRead(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	me := seedUser(t, db, tenantID)
	other := seedUser(t, db, tenantID)

	record(t, db, svc, tenantID, me, "first")
	record(t, db, svc, tenantID, me, "second")
	record(t, db, svc, tenantID, other, "not mine")

	// An empty recipient is a no-op, not an error or a row.
	if err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return svc.Record(ctx, tx, tenantID, notify.Notification{Kind: notify.KindAnswer, Title: "nobody"})
	}); err != nil {
		t.Fatalf("record with no recipient: %v", err)
	}

	count, err := svc.UnreadCount(t.Context(), tenantID, me)
	if err != nil {
		t.Fatalf("unread count: %v", err)
	}
	if count != 2 {
		t.Fatalf("unread count = %d, want 2 (the third is another person's)", count)
	}

	page, err := svc.List(t.Context(), tenantID, me, "", 20)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Notifications) != 2 {
		t.Fatalf("list returned %d, want 2 — only my own", len(page.Notifications))
	}
	// Newest first.
	if page.Notifications[0].Title != "second" {
		t.Fatalf("newest-first broken: first is %q", page.Notifications[0].Title)
	}

	// Marking one read drops the unread count; marking another's is not found.
	if err := svc.MarkRead(t.Context(), tenantID, me, page.Notifications[0].ID); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if err := svc.MarkRead(t.Context(), tenantID, other, page.Notifications[1].ID); err == nil {
		t.Fatalf("marking someone else's notification should be ErrNotFound")
	}
	count, _ = svc.UnreadCount(t.Context(), tenantID, me)
	if count != 1 {
		t.Fatalf("after one read, unread = %d, want 1", count)
	}

	// Mark all clears the rest.
	if err := svc.MarkAllRead(t.Context(), tenantID, me); err != nil {
		t.Fatalf("mark all: %v", err)
	}
	count, _ = svc.UnreadCount(t.Context(), tenantID, me)
	if count != 0 {
		t.Fatalf("after mark-all, unread = %d, want 0", count)
	}
}

func TestListPagesByKeyset(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	me := seedUser(t, db, tenantID)

	for i := range 5 {
		record(t, db, svc, tenantID, me, string(rune('a'+i)))
	}

	first, err := svc.List(t.Context(), tenantID, me, "", 2)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Notifications) != 2 || first.NextCursor == "" {
		t.Fatalf("first page: %d rows, cursor %q", len(first.Notifications), first.NextCursor)
	}

	second, err := svc.List(t.Context(), tenantID, me, first.NextCursor, 2)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Notifications) != 2 {
		t.Fatalf("second page: %d rows, want 2", len(second.Notifications))
	}
	// No overlap between pages.
	if first.Notifications[1].ID == second.Notifications[0].ID {
		t.Fatalf("pages overlap — keyset is wrong")
	}

	// A garbage cursor is a clean error, not a panic.
	if _, err := svc.List(t.Context(), tenantID, me, "not-base64!!", 2); err == nil {
		t.Fatalf("a bad cursor should error")
	}
}
