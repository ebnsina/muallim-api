package auth_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/audit"
	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/platform/database"
)

const password = "correct horse battery staple"

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testDB(t *testing.T) *database.DB {
	t.Helper()

	url := os.Getenv("MUALLIM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MUALLIM_TEST_DATABASE_URL is not set; skipping database tests")
	}

	db, err := database.New(t.Context(), database.Options{URL: url, MaxConns: 4}, discardLogger())
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
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, subdomain, name) VALUES ($1, $2, $3)`,
			id, "t"+id.String()[:8], "Test")
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

// authAuditor adapts the real recorder, exactly as cmd/ does. A stub would leave
// the audit insert and its append-only policy untested.
type authAuditor struct{ recorder *audit.Recorder }

func (a authAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e auth.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID: e.ActorID, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID,
		IP: e.IP, UserAgent: e.UserAgent, Metadata: e.Metadata,
	})
}

// sentMail is one message the service asked to have delivered.
type sentMail struct {
	Kind      string // verification, password_reset, invitation
	To        string
	Subject   string // the recipient's name, or the workspace name
	Token     string
	ExpiresIn time.Duration
}

// fakeMailer records what would have been sent.
//
// It stands in for comms.Enqueuer, whose real job is to insert a River job on
// the caller's transaction. That the insert is transactional is comms' property
// and is tested there, against a real queue. What auth owes is that a token is
// minted and handed over exactly when it should be, which is what this captures.
type fakeMailer struct {
	mu   sync.Mutex
	sent []sentMail
}

func (m *fakeMailer) SendVerification(_ context.Context, _ pgx.Tx, to, name, token string, d time.Duration) error {
	return m.record(sentMail{Kind: "verification", To: to, Subject: name, Token: token, ExpiresIn: d})
}

func (m *fakeMailer) SendPasswordReset(_ context.Context, _ pgx.Tx, to, name, token string, d time.Duration) error {
	return m.record(sentMail{Kind: "password_reset", To: to, Subject: name, Token: token, ExpiresIn: d})
}

func (m *fakeMailer) SendInvitation(_ context.Context, _ pgx.Tx, to, workspace, token string, d time.Duration) error {
	return m.record(sentMail{Kind: "invitation", To: to, Subject: workspace, Token: token, ExpiresIn: d})
}

func (m *fakeMailer) record(s sentMail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, s)
	return nil
}

// lastOf returns the most recent message of a kind sent to an address.
func (m *fakeMailer) lastOf(kind, to string) (sentMail, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := len(m.sent) - 1; i >= 0; i-- {
		if m.sent[i].Kind == kind && m.sent[i].To == to {
			return m.sent[i], true
		}
	}
	return sentMail{}, false
}

func (m *fakeMailer) countOf(kind string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	var n int
	for _, s := range m.sent {
		if s.Kind == kind {
			n++
		}
	}
	return n
}

// fakeEnqueuer records the jobs a service queues.
//
// It records rather than performs. Doing the work here would open a second
// transaction while the caller's is still holding row locks on the very sessions
// it wants to revoke, and the two would wait on each other forever. River has the
// same property for the same reason: the job runs after the commit.
type fakeEnqueuer struct {
	mu      sync.Mutex
	revoked []uuid.UUID
}

func (e *fakeEnqueuer) RevokeSessionsEverywhere(_ context.Context, _ pgx.Tx, userID uuid.UUID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.revoked = append(e.revoked, userID)
	return nil
}

func (e *fakeEnqueuer) queued(userID uuid.UUID) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, id := range e.revoked {
		if id == userID {
			return true
		}
	}
	return false
}

func newService(t *testing.T, db *database.DB) *auth.Service {
	t.Helper()
	svc, _, _ := newServiceWithFakes(t, db)
	return svc
}

func newServiceWithMailer(t *testing.T, db *database.DB) (*auth.Service, *fakeMailer) {
	t.Helper()
	svc, mail, _ := newServiceWithFakes(t, db)
	return svc, mail
}

func newServiceWithFakes(t *testing.T, db *database.DB) (*auth.Service, *fakeMailer, *fakeEnqueuer) {
	t.Helper()

	tokens, err := auth.NewTokenIssuer("a-signing-secret-of-at-least-32-bytes", "muallim-api-test")
	if err != nil {
		t.Fatal(err)
	}
	repo := auth.NewPostgresRepository()
	mail := &fakeMailer{}
	jobs := &fakeEnqueuer{}

	svc := auth.NewService(db, repo, repo, repo, tokens,
		authAuditor{audit.NewRecorder()}, mail, jobs, discardLogger())

	return svc, mail, jobs
}

func uniqueEmail() string { return "u" + uuid.NewString()[:8] + "@example.test" }

// Registration claims an unclaimed workspace. The claimant owns it.
func TestRegisterClaimsAnUnclaimedWorkspace(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)

	_, _, role, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: uniqueEmail(), Password: password}, "First", auth.RequestContext{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if role != auth.RoleOwner {
		t.Errorf("the claimant got role %q, want owner", role)
	}
}

// Once a workspace has a member, joining is by invitation. An open registration
// endpoint that answers "that email is taken" tells any stranger whether an
// address exists somewhere on the platform.
func TestRegisterIsClosedOnceTheWorkspaceIsClaimed(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)

	if _, _, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: uniqueEmail(), Password: password}, "First", auth.RequestContext{}); err != nil {
		t.Fatal(err)
	}

	// A brand-new address is refused with the same error as a taken one, so the
	// endpoint reveals nothing about which addresses exist.
	_, _, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: uniqueEmail(), Password: password}, "Second", auth.RequestContext{})
	if !errors.Is(err, auth.ErrRegistrationClosed) {
		t.Errorf("err = %v, want ErrRegistrationClosed", err)
	}
}

func TestLoginSucceedsAndNormalisesEmail(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)
	email := uniqueEmail()

	if _, _, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: email, Password: password}, "A", auth.RequestContext{}); err != nil {
		t.Fatal(err)
	}

	// Mixed case and surrounding whitespace must resolve to the same account.
	pair, user, role, err := svc.Login(t.Context(), tenantID,
		auth.Credentials{Email: "  " + strings.ToUpper(email) + " ", Password: password}, auth.RequestContext{})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if user.Email != email {
		t.Errorf("email = %q, want the normalised %q", user.Email, email)
	}
	if role != auth.RoleOwner {
		t.Errorf("role = %q, want owner", role)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Error("login returned an empty token")
	}
}

func TestLoginRejectsBadCredentialsIndistinguishably(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)
	email := uniqueEmail()

	if _, _, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: email, Password: password}, "A", auth.RequestContext{}); err != nil {
		t.Fatal(err)
	}

	tests := map[string]auth.Credentials{
		"wrong password": {Email: email, Password: "not the password at all"},
		"unknown email":  {Email: uniqueEmail(), Password: password},
	}

	for name, creds := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := svc.Login(t.Context(), tenantID, creds, auth.RequestContext{})
			if !errors.Is(err, auth.ErrInvalidCredentials) {
				t.Errorf("err = %v, want ErrInvalidCredentials — a distinguishable error is an enumeration oracle", err)
			}
		})
	}
}

// A user registered on one workspace must not be able to sign in on another.
func TestLoginIsScopedToTheTenant(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	acme := seedTenant(t, db)
	globex := seedTenant(t, db)
	email := uniqueEmail()

	if _, _, _, err := svc.Register(t.Context(), acme,
		auth.Credentials{Email: email, Password: password}, "A", auth.RequestContext{}); err != nil {
		t.Fatal(err)
	}

	_, _, _, err := svc.Login(t.Context(), globex,
		auth.Credentials{Email: email, Password: password}, auth.RequestContext{})
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("err = %v; an account on one workspace signed in on another", err)
	}
}

func TestRefreshRotatesTheToken(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)

	first, _, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: uniqueEmail(), Password: password}, "A", auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}

	second, err := svc.Refresh(t.Context(), tenantID, first.RefreshToken, auth.RequestContext{})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if second.RefreshToken == first.RefreshToken {
		t.Fatal("the refresh token was not rotated; a stolen token would stay valid forever")
	}
	if second.AccessToken == "" {
		t.Error("refresh returned no access token")
	}

	// The new token works.
	if _, err := svc.Refresh(t.Context(), tenantID, second.RefreshToken, auth.RequestContext{}); err != nil {
		t.Errorf("the rotated token did not work: %v", err)
	}
}

// The security property this design exists for. A token that arrives twice was
// stolen; we cannot tell the thief from the victim, so both are logged out.
func TestRefreshReuseRevokesTheWholeFamily(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)

	first, _, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: uniqueEmail(), Password: password}, "A", auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}

	second, err := svc.Refresh(t.Context(), tenantID, first.RefreshToken, auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}

	// The attacker replays the token that was already rotated away.
	_, err = svc.Refresh(t.Context(), tenantID, first.RefreshToken, auth.RequestContext{})
	if !errors.Is(err, auth.ErrSessionReused) {
		t.Fatalf("err = %v, want ErrSessionReused", err)
	}

	// The victim's live token must now be dead too: we cannot tell which party is
	// which, so the whole family goes.
	if _, err := svc.Refresh(t.Context(), tenantID, second.RefreshToken, auth.RequestContext{}); err == nil {
		t.Fatal("the live token still works after reuse was detected; the family was not revoked")
	}

	// And the revocation was recorded.
	var events int
	err = db.WithTenantReadOnly(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE action = $1`, auth.ActionSessionReuseDetected).Scan(&events)
	})
	if err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Errorf("audit recorded %d reuse events, want 1", events)
	}
}

func TestRefreshRejectsGarbageWithoutTouchingTheDatabase(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)

	counter := &database.Counter{}
	ctx := database.WithCounter(t.Context(), counter)

	if _, err := svc.Refresh(ctx, tenantID, "obviously-not-a-token", auth.RequestContext{}); !errors.Is(err, auth.ErrSessionInvalid) {
		t.Errorf("err = %v, want ErrSessionInvalid", err)
	}
	if counter.Count() != 0 {
		t.Errorf("a malformed token caused %d queries; it must be rejected on format alone", counter.Count())
	}
}

func TestLogoutRevokesOnlyThisSession(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)
	email := uniqueEmail()

	laptop, _, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: email, Password: password}, "A", auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}

	// A second login is a second device: a new session family.
	phone, _, _, err := svc.Login(t.Context(), tenantID,
		auth.Credentials{Email: email, Password: password}, auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}

	principal, err := svc.Verify(laptop.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Logout(t.Context(), principal, auth.RequestContext{}); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	if _, err := svc.Refresh(t.Context(), tenantID, laptop.RefreshToken, auth.RequestContext{}); err == nil {
		t.Error("the logged-out session can still refresh")
	}
	if _, err := svc.Refresh(t.Context(), tenantID, phone.RefreshToken, auth.RequestContext{}); err != nil {
		t.Errorf("logging out one device logged out the other: %v", err)
	}
}

func TestAuditTrailIsWrittenForRegistrationAndLogin(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)
	email := uniqueEmail()

	if _, _, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: email, Password: password}, "A", auth.RequestContext{}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := svc.Login(t.Context(), tenantID,
		auth.Credentials{Email: email, Password: password}, auth.RequestContext{}); err != nil {
		t.Fatal(err)
	}
	// A failed attempt is auditable too, and must not record the password.
	_, _, _, _ = svc.Login(t.Context(), tenantID,
		auth.Credentials{Email: email, Password: "wrong password entirely"}, auth.RequestContext{})

	actions := map[string]int{}
	err := db.WithTenantReadOnly(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT action, metadata::text FROM audit_log`)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var action, metadata string
			if err := rows.Scan(&action, &metadata); err != nil {
				return err
			}
			actions[action]++
			if strings.Contains(metadata, password) || strings.Contains(metadata, "wrong password entirely") {
				t.Errorf("audit metadata contains a password: %s", metadata)
			}
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{auth.ActionUserRegistered, auth.ActionUserLoggedIn, auth.ActionUserLoginFailed} {
		if actions[want] == 0 {
			t.Errorf("no audit entry for %q", want)
		}
	}
}

// The failed login is audited, which means its transaction must commit even
// though the call returns an error.
func TestFailedLoginStillCommitsItsAuditEntry(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)

	_, _, _, err := svc.Login(t.Context(), tenantID,
		auth.Credentials{Email: uniqueEmail(), Password: password}, auth.RequestContext{})
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}

	var n int
	err = db.WithTenantReadOnly(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE action = $1`, auth.ActionUserLoginFailed).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("audit_log holds %d failed-login entries, want 1 — the audit write was rolled back with the error", n)
	}
}
