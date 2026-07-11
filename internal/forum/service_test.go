package forum_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/forum"
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

// seedCourse makes a published course and returns its id and slug.
func seedCourse(t *testing.T, db *database.DB, tenantID uuid.UUID) (uuid.UUID, string) {
	t.Helper()
	id := uuid.New()
	slug := "c-" + id.String()[:8]
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO courses (tenant_id, slug, title, status) VALUES ($1, $2, 'Course', 'published') RETURNING id`,
			tenantID, slug).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed course: %v", err)
	}
	return id, slug
}

func enrol(t *testing.T, db *database.DB, tenantID, courseID, userID uuid.UUID) {
	t.Helper()
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO enrolments (tenant_id, course_id, user_id, source) VALUES ($1, $2, $3, 'self')`,
			tenantID, courseID, userID)
		return err
	})
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
}

type captureNotifier struct{ sent []forum.Notification }

func (c *captureNotifier) Notify(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n forum.Notification) error {
	c.sent = append(c.sent, n)
	return nil
}

func newService(db *database.DB, n forum.Notifier) *forum.Service {
	return forum.NewService(db, forum.NewPostgresRepository(), n)
}

func mod(id uuid.UUID) forum.Actor  { return forum.Actor{UserID: id, CanModerate: true} }
func user(id uuid.UUID) forum.Actor { return forum.Actor{UserID: id} }

func TestCourseSpaceFollowsEnrolment(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db, nil)
	tenantID := seedTenant(t, db)
	courseID, slug := seedCourse(t, db, tenantID)

	moderator := seedUser(t, db, tenantID)
	member := seedUser(t, db, tenantID)
	outsider := seedUser(t, db, tenantID)

	// A moderator opens a workspace space and a course space.
	ws, err := svc.CreateSpace(t.Context(), tenantID, "", "Commons", "everyone")
	if err != nil {
		t.Fatalf("create workspace space: %v", err)
	}
	cs, err := svc.CreateSpace(t.Context(), tenantID, slug, "Course chat", "for the course")
	if err != nil {
		t.Fatalf("create course space: %v", err)
	}
	if ws.Workspace() != true || cs.Workspace() != false {
		t.Fatalf("space kinds wrong: ws.Workspace=%v cs.Workspace=%v", ws.Workspace(), cs.Workspace())
	}

	// The outsider sees only the workspace space; the enrolled member sees both.
	enrol(t, db, tenantID, courseID, member)

	outsiderSpaces, _ := svc.Spaces(t.Context(), tenantID, user(outsider))
	if len(outsiderSpaces) != 1 {
		t.Fatalf("outsider saw %d spaces, want 1 (workspace only)", len(outsiderSpaces))
	}
	memberSpaces, _ := svc.Spaces(t.Context(), tenantID, user(member))
	if len(memberSpaces) != 2 {
		t.Fatalf("member saw %d spaces, want 2", len(memberSpaces))
	}
	modSpaces, _ := svc.Spaces(t.Context(), tenantID, mod(moderator))
	if len(modSpaces) != 2 {
		t.Fatalf("moderator saw %d spaces, want 2", len(modSpaces))
	}

	// The outsider cannot start a thread in the course space — it is not found.
	if _, err := svc.StartThread(t.Context(), tenantID, cs.ID, user(outsider), "hi", "there"); err != forum.ErrSpaceNotFound {
		t.Fatalf("outsider start thread: got %v, want ErrSpaceNotFound", err)
	}
	// The member can.
	if _, err := svc.StartThread(t.Context(), tenantID, cs.ID, user(member), "Question", "body"); err != nil {
		t.Fatalf("member start thread: %v", err)
	}
}

func TestReplyRollsUpAndNotifiesAndLocks(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	cap := &captureNotifier{}
	svc := newService(db, cap)
	tenantID := seedTenant(t, db)

	author := seedUser(t, db, tenantID)
	replier := seedUser(t, db, tenantID)

	ws, err := svc.CreateSpace(t.Context(), tenantID, "", "Commons", "")
	if err != nil {
		t.Fatalf("create space: %v", err)
	}
	thread, err := svc.StartThread(t.Context(), tenantID, ws.ID, user(author), "Hello", "opening")
	if err != nil {
		t.Fatalf("start thread: %v", err)
	}

	// A reply from someone else notifies the author and bumps the count.
	if _, err := svc.Reply(t.Context(), tenantID, thread.ID, user(replier), "first reply"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if len(cap.sent) != 1 || cap.sent[0].UserID != author {
		t.Fatalf("reply notification wrong: %+v", cap.sent)
	}

	got, page, err := svc.Thread(t.Context(), tenantID, thread.ID, user(author), "", 20)
	if err != nil {
		t.Fatalf("read thread: %v", err)
	}
	if got.ReplyCount != 1 || len(page.Posts) != 1 {
		t.Fatalf("roll-up wrong: reply_count=%d posts=%d", got.ReplyCount, len(page.Posts))
	}

	// The author replying to their own thread notifies nobody new.
	if _, err := svc.Reply(t.Context(), tenantID, thread.ID, user(author), "self"); err != nil {
		t.Fatalf("self reply: %v", err)
	}
	if len(cap.sent) != 1 {
		t.Fatalf("self-reply should not notify: now %d", len(cap.sent))
	}

	// Locking stops non-moderator replies; a moderator may still post.
	if err := svc.Lock(t.Context(), tenantID, thread.ID, true); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if _, err := svc.Reply(t.Context(), tenantID, thread.ID, user(replier), "after lock"); err != forum.ErrLocked {
		t.Fatalf("reply to locked: got %v, want ErrLocked", err)
	}
	if _, err := svc.Reply(t.Context(), tenantID, thread.ID, mod(replier), "moderator override"); err != nil {
		t.Fatalf("moderator reply to locked: %v", err)
	}
}

// The thread-with-posts read is a fixed number of queries whatever the reply
// count, or a busy thread becomes one query per reply.
func TestThreadReadIsNotNPlusOne(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db, nil)
	tenantID := seedTenant(t, db)
	member := seedUser(t, db, tenantID)

	ws, _ := svc.CreateSpace(t.Context(), tenantID, "", "Commons", "")
	thread, _ := svc.StartThread(t.Context(), tenantID, ws.ID, user(member), "T", "b")

	count := func() int {
		counter := &database.Counter{}
		ctx := database.WithCounter(t.Context(), counter)
		if _, _, err := svc.Thread(ctx, tenantID, thread.ID, user(member), "", 50); err != nil {
			t.Fatalf("read: %v", err)
		}
		return counter.Count()
	}

	reply := func(n int) {
		for range n {
			if _, err := svc.Reply(t.Context(), tenantID, thread.ID, user(member), "r"); err != nil {
				t.Fatalf("reply: %v", err)
			}
		}
	}

	reply(2)
	small := count()
	reply(8)
	large := count()
	if small != large {
		t.Fatalf("query count grew with replies: %d then %d — an N+1", small, large)
	}
}

func TestThreadsPageByKeysetPinnedFirst(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db, nil)
	tenantID := seedTenant(t, db)
	member := seedUser(t, db, tenantID)

	ws, _ := svc.CreateSpace(t.Context(), tenantID, "", "Commons", "")
	var pinnedID uuid.UUID
	for i := range 5 {
		th, err := svc.StartThread(t.Context(), tenantID, ws.ID, user(member), "T", "b")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		if i == 0 {
			pinnedID = th.ID // the oldest, pinned, must still lead
			if err := svc.Pin(t.Context(), tenantID, th.ID, true); err != nil {
				t.Fatalf("pin: %v", err)
			}
		}
	}

	_, first, err := svc.Threads(t.Context(), tenantID, ws.ID, user(member), "", 2)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Threads) != 2 || first.NextCursor == "" {
		t.Fatalf("first page: %d threads, cursor %q", len(first.Threads), first.NextCursor)
	}
	if first.Threads[0].ID != pinnedID || !first.Threads[0].Pinned {
		t.Fatalf("pinned thread does not lead: %v", first.Threads[0])
	}

	_, second, err := svc.Threads(t.Context(), tenantID, ws.ID, user(member), first.NextCursor, 2)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Threads) != 2 {
		t.Fatalf("second page: %d, want 2", len(second.Threads))
	}
	if first.Threads[1].ID == second.Threads[0].ID {
		t.Fatalf("pages overlap")
	}
}
