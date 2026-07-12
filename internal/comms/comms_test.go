package comms_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/ebnsina/muallim-api/internal/comms"
)

// testPool connects to the database `make test-db` provides, and skips when there
// is none. A queue backed by Postgres cannot be tested against a fake: the whole
// property under test is what the transaction does.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("MUALLIM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MUALLIM_TEST_DATABASE_URL is not set")
	}

	pool, err := pgxpool.New(t.Context(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newEnqueuer builds an insert-only Enqueuer, exactly as cmd/api does: the driver
// gets no pool, so only the transactional insert is available.
func newEnqueuer(t *testing.T, baseURL string) *comms.Enqueuer {
	t.Helper()

	client, err := river.NewClient(riverpgxv5.New(nil), &river.Config{})
	if err != nil {
		t.Fatalf("river client: %v", err)
	}

	enq, err := comms.NewEnqueuer(client, baseURL)
	if err != nil {
		t.Fatalf("enqueuer: %v", err)
	}
	return enq
}

// recipient returns an address nobody else will use.
//
// river_job is not cleaned between runs, and the end-to-end suite shares this
// database, so a query for "every email job" answers with everything anyone has
// ever queued. A test that counts rows must count its own.
func recipient(t *testing.T) string {
	t.Helper()
	return uuid.NewString() + "@example.test"
}

// jobsIn counts the email jobs addressed to `to` and visible on tx, and returns
// the newest one's args.
func jobsIn(t *testing.T, ctx context.Context, tx pgx.Tx, to string) (int, comms.EmailArgs) {
	t.Helper()

	rows, err := tx.Query(ctx,
		`SELECT args FROM river_job WHERE kind = $1 AND args->>'to' = $2 ORDER BY id DESC`,
		comms.EmailArgs{}.Kind(), to)
	if err != nil {
		t.Fatalf("query jobs: %v", err)
	}
	defer rows.Close()

	var (
		n      int
		newest comms.EmailArgs
	)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan args: %v", err)
		}
		if n == 0 {
			if err := json.Unmarshal(raw, &newest); err != nil {
				t.Fatalf("unmarshal args: %v", err)
			}
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return n, newest
}

// The job is inserted on the caller's transaction, carries the rendered message,
// and embeds the token in a link into the web client.
func TestSendPasswordResetEnqueuesOnTheTransaction(t *testing.T) {
	t.Parallel()

	pool := testPool(t)
	enq := newEnqueuer(t, "https://app.example.com")

	to := recipient(t)

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()

	const token = "a-token-that-looks-opaque"
	if err := enq.SendPasswordReset(t.Context(), tx, to, "Ada", token, time.Hour); err != nil {
		t.Fatalf("send: %v", err)
	}

	n, args := jobsIn(t, t.Context(), tx, to)
	if n != 1 {
		t.Fatalf("jobs enqueued = %d, want 1", n)
	}

	if args.To != to {
		t.Errorf("to = %q", args.To)
	}
	if args.Subject != "Reset your password" {
		t.Errorf("subject = %q", args.Subject)
	}

	want := "https://app.example.com/reset-password?token=" + token
	if !strings.Contains(args.Text, want) {
		t.Errorf("text body does not carry the link %q:\n%s", want, args.Text)
	}
	// The HTML escapes the ampersand-free URL identically here, but the token must
	// be present in both bodies or one of them is a dead link.
	if !strings.Contains(args.HTML, token) {
		t.Errorf("html body does not carry the token:\n%s", args.HTML)
	}
	if !strings.Contains(args.Text, "1 hour") {
		t.Errorf("text body does not state the expiry:\n%s", args.Text)
	}
}

// The reason River is backed by Postgres: a job enqueued by a transaction that
// rolls back was never enqueued. Otherwise a reset email goes out for a reset
// token that does not exist.
func TestEnqueuedJobDiesWithItsTransaction(t *testing.T) {
	t.Parallel()

	pool := testPool(t)
	enq := newEnqueuer(t, "https://app.example.com")

	to := recipient(t)

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	// Rolled back even if an assertion below ends the test. Without this, a
	// failure leaves the transaction open, the connection checked out, and
	// pool.Close() waiting on it until the whole package times out — which is a
	// far more confusing way to learn that one count was wrong.
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := enq.SendVerification(t.Context(), tx, to, "Ada", "tok", 24*time.Hour); err != nil {
		t.Fatalf("send: %v", err)
	}
	if n, _ := jobsIn(t, t.Context(), tx, to); n != 1 {
		t.Fatalf("jobs visible inside the transaction = %d, want 1", n)
	}

	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// A fresh transaction must not see it.
	after, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = after.Rollback(t.Context()) }()

	if n, _ := jobsIn(t, t.Context(), after, to); n != 0 {
		t.Errorf("a rolled-back transaction left %d jobs behind, want 0", n)
	}
}

// An invitation names the workspace, and the name is escaped into the HTML rather
// than interpolated raw.
func TestSendInvitationEscapesTheWorkspaceName(t *testing.T) {
	t.Parallel()

	pool := testPool(t)
	enq := newEnqueuer(t, "https://app.example.com")

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()

	to := recipient(t)

	const evil = `Acme <script>alert(1)</script>`
	if err := enq.SendInvitation(t.Context(), tx, to, evil, "tok", 7*24*time.Hour); err != nil {
		t.Fatalf("send: %v", err)
	}

	_, args := jobsIn(t, t.Context(), tx, to)
	if strings.Contains(args.HTML, "<script>") {
		t.Errorf("workspace name reached the html unescaped:\n%s", args.HTML)
	}
	if !strings.Contains(args.Text, "7 days") {
		t.Errorf("text body does not state the expiry:\n%s", args.Text)
	}
}

// A base URL that is not absolute produces links nobody can click. Refuse it at
// construction, where a person is looking, rather than in an inbox.
func TestNewEnqueuerRefusesARelativeBaseURL(t *testing.T) {
	t.Parallel()

	client, err := river.NewClient(riverpgxv5.New(nil), &river.Config{})
	if err != nil {
		t.Fatalf("river client: %v", err)
	}

	for _, base := range []string{"", "/app", "app.example.com"} {
		if _, err := comms.NewEnqueuer(client, base); err == nil {
			t.Errorf("NewEnqueuer(%q) accepted a base URL that is not absolute", base)
		}
	}
}

func TestNewEnqueuerRefusesANilClient(t *testing.T) {
	t.Parallel()

	if _, err := comms.NewEnqueuer(nil, "https://app.example.com"); err == nil {
		t.Error("NewEnqueuer accepted a nil client")
	}
}
