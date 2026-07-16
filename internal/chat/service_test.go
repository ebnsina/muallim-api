package chat_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/chat"
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
	sub := "c" + id.String()[:8]
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

func seedUser(t *testing.T, db *database.DB, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	email := id.String() + "@example.test"
	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO users (id, email, password_hash, name) VALUES ($1, $2, '', $3)`, id, email, name)
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

func seedCourse(t *testing.T, db *database.DB, tenant uuid.UUID, slug string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := db.WithTenant(t.Context(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO courses (tenant_id, slug, title, status) VALUES ($1, $2, $3, 'published') RETURNING id`,
			tenant, slug, "Course "+slug).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed course: %v", err)
	}
	return id
}

type stubAuditor struct{}

func (stubAuditor) Record(context.Context, pgx.Tx, uuid.UUID, chat.AuditEntry) error { return nil }

func newService(db *database.DB) *chat.Service {
	return chat.NewService(db, chat.NewPostgresRepository(), stubAuditor{})
}

func TestDirectIsFindOrCreateAndOrderIndependent(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	alice := seedUser(t, db, "Alice")
	bob := seedUser(t, db, "Bob")

	first, err := svc.EnsureDirect(t.Context(), tenant, alice, bob)
	if err != nil {
		t.Fatalf("ensure direct: %v", err)
	}
	if first.Kind != chat.KindDirect {
		t.Fatalf("kind %q, want direct", first.Kind)
	}

	// The same pair the other way round resolves to the same conversation.
	again, err := svc.EnsureDirect(t.Context(), tenant, bob, alice)
	if err != nil {
		t.Fatalf("ensure direct reversed: %v", err)
	}
	if again.ID != first.ID {
		t.Fatalf("reversed pair made a new conversation %s, want %s", again.ID, first.ID)
	}

	// Both members are in it, and both may send.
	full, err := svc.Conversation(t.Context(), tenant, first.ID)
	if err != nil {
		t.Fatalf("load conversation: %v", err)
	}
	if len(full.Members) != 2 {
		t.Fatalf("direct has %d members, want 2", len(full.Members))
	}

	// A direct conversation with oneself is refused.
	if _, err := svc.EnsureDirect(t.Context(), tenant, alice, alice); !errors.Is(err, chat.ErrInvalid) {
		t.Fatalf("self-direct accepted: %v", err)
	}
}

func TestCreateGroupAddsCreatorAsAdmin(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	owner := seedUser(t, db, "Owner")
	m1 := seedUser(t, db, "One")
	m2 := seedUser(t, db, "Two")

	conv, err := svc.CreateGroup(t.Context(), tenant, "Study Group", []uuid.UUID{m1, m2}, owner)
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	full, err := svc.Conversation(t.Context(), tenant, conv.ID)
	if err != nil {
		t.Fatalf("load group: %v", err)
	}
	if len(full.Members) != 3 {
		t.Fatalf("group has %d members, want 3", len(full.Members))
	}
	var ownerRole string
	for _, m := range full.Members {
		if m.UserID == owner {
			ownerRole = m.Role
		}
	}
	if ownerRole != chat.RoleAdmin {
		t.Fatalf("creator role %q, want admin", ownerRole)
	}

	// A group with no other members is refused.
	if _, err := svc.CreateGroup(t.Context(), tenant, "Empty", nil, owner); !errors.Is(err, chat.ErrInvalid) {
		t.Fatalf("empty group accepted: %v", err)
	}
}

func TestSendRequiresMembershipAndMessagesPaginate(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	alice := seedUser(t, db, "Alice")
	bob := seedUser(t, db, "Bob")
	outsider := seedUser(t, db, "Outsider")

	conv, err := svc.EnsureDirect(t.Context(), tenant, alice, bob)
	if err != nil {
		t.Fatalf("ensure direct: %v", err)
	}

	// An outsider may not send.
	if _, err := svc.Send(t.Context(), tenant, conv.ID, outsider, "hi"); !errors.Is(err, chat.ErrNotMember) {
		t.Fatalf("outsider send accepted: %v", err)
	}
	// Sending to an unknown conversation is not found.
	if _, err := svc.Send(t.Context(), tenant, uuid.New(), alice, "hi"); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("send to unknown conversation: %v", err)
	}
	// An empty body is refused.
	if _, err := svc.Send(t.Context(), tenant, conv.ID, alice, "   "); !errors.Is(err, chat.ErrInvalid) {
		t.Fatalf("empty body accepted: %v", err)
	}

	// A member sends a handful of messages.
	for i := 0; i < 5; i++ {
		if _, err := svc.Send(t.Context(), tenant, conv.ID, alice, "msg"); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	// Keyset paginate two at a time; every message is seen exactly once.
	seen := map[uuid.UUID]bool{}
	var cursor string
	for pages := 0; pages < 10; pages++ {
		page, err := svc.Messages(t.Context(), tenant, conv.ID, bob, chat.PageParams{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("messages page: %v", err)
		}
		for _, m := range page.Messages {
			if seen[m.ID] {
				t.Fatalf("message %s returned twice", m.ID)
			}
			seen[m.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("saw %d distinct messages, want 5", len(seen))
	}

	// An outsider may not read either.
	if _, err := svc.Messages(t.Context(), tenant, conv.ID, outsider, chat.PageParams{}); !errors.Is(err, chat.ErrNotMember) {
		t.Fatalf("outsider read accepted: %v", err)
	}
}

func TestMarkReadAndUnreadCount(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	alice := seedUser(t, db, "Alice")
	bob := seedUser(t, db, "Bob")

	conv, err := svc.EnsureDirect(t.Context(), tenant, alice, bob)
	if err != nil {
		t.Fatalf("ensure direct: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := svc.Send(t.Context(), tenant, conv.ID, alice, "hello"); err != nil {
			t.Fatalf("send: %v", err)
		}
	}

	// Bob has three unread from Alice.
	page, err := svc.ListForUser(t.Context(), tenant, bob, chat.PageParams{})
	if err != nil {
		t.Fatalf("list for bob: %v", err)
	}
	if len(page.Conversations) != 1 || page.Conversations[0].Unread != 3 {
		t.Fatalf("bob unread = %+v, want one conversation with 3", page.Conversations)
	}
	if page.Conversations[0].LastMessage == nil {
		t.Fatalf("conversation has no last message")
	}

	// After marking read, unread is zero.
	if err := svc.MarkRead(t.Context(), tenant, conv.ID, bob); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	page, err = svc.ListForUser(t.Context(), tenant, bob, chat.PageParams{})
	if err != nil {
		t.Fatalf("list for bob again: %v", err)
	}
	if page.Conversations[0].Unread != 0 {
		t.Fatalf("unread after read = %d, want 0", page.Conversations[0].Unread)
	}

	// A non-member cannot mark read.
	outsider := seedUser(t, db, "Outsider")
	if err := svc.MarkRead(t.Context(), tenant, conv.ID, outsider); !errors.Is(err, chat.ErrNotMember) {
		t.Fatalf("outsider mark read accepted: %v", err)
	}
}

func TestListForUserReturnsOnlyMine(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	alice := seedUser(t, db, "Alice")
	bob := seedUser(t, db, "Bob")
	carol := seedUser(t, db, "Carol")

	// Alice-Bob and a Bob-Carol conversation. Alice sees only her own.
	if _, err := svc.EnsureDirect(t.Context(), tenant, alice, bob); err != nil {
		t.Fatalf("ab: %v", err)
	}
	if _, err := svc.EnsureDirect(t.Context(), tenant, bob, carol); err != nil {
		t.Fatalf("bc: %v", err)
	}
	page, err := svc.ListForUser(t.Context(), tenant, alice, chat.PageParams{})
	if err != nil {
		t.Fatalf("list for alice: %v", err)
	}
	if len(page.Conversations) != 1 {
		t.Fatalf("alice sees %d conversations, want 1", len(page.Conversations))
	}
}

func TestCourseChannelIsGetOrCreate(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	learner := seedUser(t, db, "Learner")
	course := seedCourse(t, db, tenant, "algebra-1")

	resolved, err := svc.CourseIDBySlug(t.Context(), tenant, "algebra-1")
	if err != nil {
		t.Fatalf("resolve slug: %v", err)
	}
	if resolved != course {
		t.Fatalf("resolved %s, want %s", resolved, course)
	}

	first, err := svc.EnsureCourseChannel(t.Context(), tenant, course, "Algebra 1")
	if err != nil {
		t.Fatalf("ensure channel: %v", err)
	}
	// A second call returns the same channel, not a duplicate.
	again, err := svc.EnsureCourseChannel(t.Context(), tenant, course, "Algebra 1")
	if err != nil {
		t.Fatalf("ensure channel again: %v", err)
	}
	if again.ID != first.ID {
		t.Fatalf("course got a second channel %s, want %s", again.ID, first.ID)
	}

	// The learner joins, then may send.
	if err := svc.EnsureMember(t.Context(), tenant, first.ID, learner); err != nil {
		t.Fatalf("ensure member: %v", err)
	}
	if err := svc.EnsureMember(t.Context(), tenant, first.ID, learner); err != nil {
		t.Fatalf("ensure member idempotent: %v", err)
	}
	if _, err := svc.Send(t.Context(), tenant, first.ID, learner, "salaam"); err != nil {
		t.Fatalf("member send: %v", err)
	}

	// An unknown conversation is not found.
	if _, err := svc.Conversation(t.Context(), tenant, uuid.New()); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("unknown conversation: %v", err)
	}
}
