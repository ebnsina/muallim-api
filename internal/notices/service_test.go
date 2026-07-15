package notices_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/notices"
	"github.com/ebnsina/muallim-api/internal/platform/database"
)

func testDB(t *testing.T) *database.DB {
	t.Helper()
	url := os.Getenv("MUALLIM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MUALLIM_TEST_DATABASE_URL is not set")
	}
	db, err := database.New(t.Context(), database.Options{URL: url, MaxConns: 4},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func seedTenant(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	sub := "n" + id.String()[:8]
	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, subdomain, name) VALUES ($1, $2, $3)`, id, sub, "Test "+sub)
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_ = db.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, id)
			return err
		})
	})
	return id
}

// seedGuardianOf admits a student in a class and links a guardian with the given
// email and phone (either may be blank).
func seedGuardianOf(t *testing.T, db *database.DB, tenant, class uuid.UUID, admissionNo, guardianEmail, guardianPhone string) {
	t.Helper()
	err := db.WithTenant(t.Context(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		var studentID, guardianID uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO students (tenant_id, admission_no, full_name, grade_level_id) VALUES ($1, $2, $3, $4) RETURNING id`,
			tenant, admissionNo, "Student "+admissionNo, class).Scan(&studentID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO guardians (tenant_id, full_name, email, phone) VALUES ($1, $2, $3, $4) RETURNING id`,
			tenant, "Guardian of "+admissionNo, guardianEmail, guardianPhone).Scan(&guardianID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO student_guardians (tenant_id, student_id, guardian_id, is_primary) VALUES ($1, $2, $3, true)`,
			tenant, studentID, guardianID)
		return err
	})
	if err != nil {
		t.Fatalf("seed guardian: %v", err)
	}
}

func seedUser(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			id, "u"+id.String()[:8]+"@example.test")
		return err
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_ = db.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
			return err
		})
	})
	return id
}

func seedClass(t *testing.T, db *database.DB, tenant uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := db.WithTenant(t.Context(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO grade_levels (tenant_id, name, rank) VALUES ($1, 'Class 6', 6) RETURNING id`, tenant).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed class: %v", err)
	}
	return id
}

// countingBroadcaster records every recipient it was asked to send to, per channel.
type countingBroadcaster struct {
	mu    sync.Mutex
	sent  []string
	texts []string
}

func (b *countingBroadcaster) SendEmail(ctx context.Context, tx pgx.Tx, to, name, subject, body string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, to)
	return nil
}

func (b *countingBroadcaster) SendSMS(ctx context.Context, tx pgx.Tx, to, text string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.texts = append(b.texts, to)
	return nil
}

type stubAuditor struct{}

func (stubAuditor) Record(context.Context, pgx.Tx, uuid.UUID, notices.AuditEntry) error { return nil }

func TestPostNoticeFansOut(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	class := seedClass(t, db, tenant)
	seedGuardianOf(t, db, tenant, class, "2025-001", "parent1@example.test", "01700000001")
	seedGuardianOf(t, db, tenant, class, "2025-002", "parent2@example.test", "")

	bc := &countingBroadcaster{}
	svc := notices.NewService(db, notices.NewPostgresRepository(), bc, stubAuditor{})
	author := notices.Author{UserID: seedUser(t, db)}

	notice, err := svc.Post(t.Context(), tenant, notices.NewNotice{
		Title: "School closed Thursday", Body: "The school will be closed for the holiday.",
		Audience: notices.AudienceClass, TargetID: &class,
	}, author)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if notice.RecipientCount != 2 {
		t.Fatalf("recipient count %d, want 2", notice.RecipientCount)
	}
	if len(bc.sent) != 2 {
		t.Fatalf("email broadcast to %d, want 2", len(bc.sent))
	}
	if notice.Channel != notices.ChannelEmail {
		t.Fatalf("channel %q, want the email default", notice.Channel)
	}

	// An SMS notice reaches only the guardian with a phone.
	bc2 := &countingBroadcaster{}
	svc2 := notices.NewService(db, notices.NewPostgresRepository(), bc2, stubAuditor{})
	smsNotice, err := svc2.Post(t.Context(), tenant, notices.NewNotice{
		Title: "Reminder", Body: "Fees due Friday.", Audience: notices.AudienceClass,
		TargetID: &class, Channel: notices.ChannelSMS,
	}, author)
	if err != nil {
		t.Fatalf("post sms: %v", err)
	}
	if smsNotice.RecipientCount != 1 || len(bc2.texts) != 1 || len(bc2.sent) != 0 {
		t.Fatalf("sms notice reached email=%d sms=%d count=%d, want only the one phone",
			len(bc2.sent), len(bc2.texts), smsNotice.RecipientCount)
	}

	// Both notices are on the board, newest first.
	page, err := svc.Board(t.Context(), tenant, notices.PageParams{Limit: 50})
	if err != nil {
		t.Fatalf("board: %v", err)
	}
	if len(page.Notices) != 2 || page.Notices[0].Title != "Reminder" {
		t.Fatalf("board is %+v, want the two notices newest first", page.Notices)
	}
}

func TestPostNoticeRefusesEmptyAudience(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)

	bc := &countingBroadcaster{}
	svc := notices.NewService(db, notices.NewPostgresRepository(), bc, stubAuditor{})

	// No guardians at all: the notice is refused and nothing is sent or stored.
	if _, err := svc.Post(t.Context(), tenant, notices.NewNotice{
		Title: "Hello", Body: "Anyone?", Audience: notices.AudienceAll,
	}, notices.Author{UserID: uuid.New()}); !errors.Is(err, notices.ErrNoRecipients) {
		t.Fatalf("an empty audience was accepted: %v", err)
	}
	if len(bc.sent) != 0 {
		t.Fatalf("sent %d despite empty audience", len(bc.sent))
	}

	// A class audience with no class named is refused.
	if _, err := svc.Post(t.Context(), tenant, notices.NewNotice{
		Title: "Hi", Body: "x", Audience: notices.AudienceClass,
	}, notices.Author{UserID: uuid.New()}); !errors.Is(err, notices.ErrTargetRequired) {
		t.Fatalf("a class notice without a class was accepted: %v", err)
	}
}
