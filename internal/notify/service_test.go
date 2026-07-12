package notify_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/notify"
	"github.com/ebnsina/muallim-api/internal/platform/database"
)

func testDB(t *testing.T) *database.DB {
	t.Helper()
	url := os.Getenv("MUALLIM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MUALLIM_TEST_DATABASE_URL is not set; skipping database tests")
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
	return notify.NewService(db, notify.NewPostgresRepository(), nil)
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

// seedCourseWithLearners makes a published course with two active enrolments and
// one cancelled one, and returns the course id and the two active learners.
func seedCourseWithLearners(t *testing.T, db *database.DB, tenantID uuid.UUID) (courseID uuid.UUID, active []uuid.UUID) {
	t.Helper()
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO courses (tenant_id, slug, title, status) VALUES ($1, $2, 'C', 'published') RETURNING id`,
			tenantID, "c-"+uuid.NewString()[:8]).Scan(&courseID); err != nil {
			return err
		}
		for _, status := range []string{"active", "completed", "cancelled"} {
			u := seedUser(t, db, tenantID)
			if _, err := tx.Exec(ctx,
				`INSERT INTO enrolments (tenant_id, course_id, user_id, source, status) VALUES ($1, $2, $3, 'self', $4)`,
				tenantID, courseID, u, status); err != nil {
				return err
			}
			if status != "cancelled" {
				active = append(active, u)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed course with learners: %v", err)
	}
	return courseID, active
}

func TestFanOutReachesActiveLearnersOnceAndIsIdempotent(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	courseID, active := seedCourseWithLearners(t, db, tenantID)
	announcementID := uuid.New()

	created, err := svc.FanOutAnnouncement(t.Context(), tenantID, courseID, announcementID,
		"Exam moved", "It's next Friday.", "/courses/c")
	if err != nil {
		t.Fatalf("fan out: %v", err)
	}
	// Two active/completed learners; the cancelled one is not reached.
	if created != 2 {
		t.Fatalf("fan-out created %d notifications, want 2", created)
	}
	for _, learner := range active {
		count, _ := svc.UnreadCount(t.Context(), tenantID, learner)
		if count != 1 {
			t.Fatalf("learner %v has %d unread, want 1", learner, count)
		}
	}

	// Running the same fan-out again — as a retried job would — creates nothing new.
	again, err := svc.FanOutAnnouncement(t.Context(), tenantID, courseID, announcementID,
		"Exam moved", "It's next Friday.", "/courses/c")
	if err != nil {
		t.Fatalf("re-run fan out: %v", err)
	}
	if again != 0 {
		t.Fatalf("idempotent fan-out created %d on re-run, want 0", again)
	}
}

// seedUserEmail seeds a user with a chosen email, so a digest test can assert who
// was mailed by address.
func seedUserEmail(t *testing.T, db *database.DB, tenantID uuid.UUID, email string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO users (id, email, password_hash, name) VALUES ($1, $2, 'x', 'Learner')`,
			id, email); err != nil {
			return err
		}
		// The user must be a member of the tenant to be visible under its RLS — the
		// same as every real account the digest would mail.
		_, err := tx.Exec(ctx,
			`INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'student')`, tenantID, id)
		return err
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

type captureMailer struct {
	sent []struct{ to, subject, text string }
}

func (m *captureMailer) SendRendered(ctx context.Context, tx pgx.Tx, to, subject, text string) error {
	m.sent = append(m.sent, struct{ to, subject, text string }{to, subject, text})
	return nil
}

func (m *captureMailer) to(addr string) (int, string) {
	n, subject := 0, ""
	for _, e := range m.sent {
		if e.to == addr {
			n++
			subject = e.subject
		}
	}
	return n, subject
}

func TestDigestMailsUnreadRespectsOptOutAndIsIdempotent(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenantID := seedTenant(t, db)

	mailer := &captureMailer{}
	svc := notify.NewService(db, notify.NewPostgresRepository(), mailer)
	plain := notify.NewService(db, notify.NewPostgresRepository(), nil)

	inAddr := "digest-in-" + uuid.NewString()[:8] + "@example.test"
	outAddr := "digest-out-" + uuid.NewString()[:8] + "@example.test"
	optedIn := seedUserEmail(t, db, tenantID, inAddr)
	optedOut := seedUserEmail(t, db, tenantID, outAddr)

	record(t, db, plain, tenantID, optedIn, "first")
	record(t, db, plain, tenantID, optedIn, "second")
	record(t, db, plain, tenantID, optedOut, "for the opted-out one")

	// The opted-out learner turns the digest off.
	if err := plain.SetPreferences(t.Context(), tenantID, optedOut, notify.Preferences{EmailDigest: false}); err != nil {
		t.Fatalf("set prefs: %v", err)
	}

	if _, err := svc.SendDigests(t.Context()); err != nil {
		t.Fatalf("send digests: %v", err)
	}

	if n, subject := mailer.to(inAddr); n != 1 || subject != "You have 2 new notifications" {
		t.Fatalf("opted-in got %d mails, subject %q", n, subject)
	}
	if n, _ := mailer.to(outAddr); n != 0 {
		t.Fatalf("opted-out got %d mails, want 0", n)
	}

	// Running again mails no one who was already digested.
	if _, err := svc.SendDigests(t.Context()); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if n, _ := mailer.to(inAddr); n != 1 {
		t.Fatalf("idempotent digest mailed %d times, want 1", n)
	}
}
