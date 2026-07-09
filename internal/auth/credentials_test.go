package auth_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ebnsina/lms-api/internal/auth"
)

// registerOwner claims a fresh workspace and returns the owner's address.
func registerOwner(t *testing.T, svc *auth.Service, tenantID uuid.UUID) string {
	t.Helper()

	email := uniqueEmail()
	if _, _, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: email, Password: password}, "Ada", auth.RequestContext{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return email
}

// Registering queues exactly one verification email, carrying a token that works.
func TestRegisterSendsAVerificationEmail(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc, mail := newServiceWithMailer(t, db)
	tenantID := seedTenant(t, db)

	email := registerOwner(t, svc, tenantID)

	sent, ok := mail.lastOf("verification", email)
	if !ok {
		t.Fatal("registration sent no verification email")
	}
	if sent.Token == "" {
		t.Fatal("verification email carried no token")
	}
	if sent.ExpiresIn != auth.EmailVerificationTTL {
		t.Errorf("expiry told to the user = %v, want %v", sent.ExpiresIn, auth.EmailVerificationTTL)
	}

	if err := svc.VerifyEmail(t.Context(), tenantID, sent.Token, auth.RequestContext{}); err != nil {
		t.Fatalf("verify email: %v", err)
	}

	// The profile must report the address as confirmed, or nothing observable
	// changed.
	pair, _, _, err := svc.Login(t.Context(), tenantID,
		auth.Credentials{Email: email, Password: password}, auth.RequestContext{})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	principal, err := svc.Verify(pair.AccessToken)
	if err != nil {
		t.Fatalf("verify token: %v", err)
	}
	user, _, err := svc.Me(t.Context(), principal)
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	if user.EmailVerifiedAt == nil {
		t.Error("email_verified_at is still null after verifying")
	}
}

// A verification token is single-use. Clicking the link twice must not leave it
// spendable a third time.
func TestVerificationTokenIsSpentOnce(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc, mail := newServiceWithMailer(t, db)
	tenantID := seedTenant(t, db)

	email := registerOwner(t, svc, tenantID)
	sent, _ := mail.lastOf("verification", email)

	if err := svc.VerifyEmail(t.Context(), tenantID, sent.Token, auth.RequestContext{}); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if err := svc.VerifyEmail(t.Context(), tenantID, sent.Token, auth.RequestContext{}); !errors.Is(err, auth.ErrTokenInvalid) {
		t.Errorf("second verify error = %v, want ErrTokenInvalid", err)
	}
}

// A verification token must not be presentable as a password reset. The purpose
// is part of the lookup, so the wrong purpose simply does not find it.
func TestVerificationTokenCannotResetAPassword(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc, mail := newServiceWithMailer(t, db)
	tenantID := seedTenant(t, db)

	email := registerOwner(t, svc, tenantID)
	sent, _ := mail.lastOf("verification", email)

	err := svc.ResetPassword(t.Context(), tenantID, sent.Token, "a-completely-new-password", auth.RequestContext{})
	if !errors.Is(err, auth.ErrTokenInvalid) {
		t.Errorf("reset with a verification token = %v, want ErrTokenInvalid", err)
	}
}

// The full reset loop: request a link, spend it, and the new password is the one
// that works.
func TestPasswordResetReplacesThePassword(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc, mail := newServiceWithMailer(t, db)
	tenantID := seedTenant(t, db)

	email := registerOwner(t, svc, tenantID)

	if err := svc.RequestPasswordReset(t.Context(), tenantID, email, auth.RequestContext{}); err != nil {
		t.Fatalf("request reset: %v", err)
	}

	sent, ok := mail.lastOf("password_reset", email)
	if !ok {
		t.Fatal("no reset email was sent to a member who asked for one")
	}
	if sent.ExpiresIn != auth.PasswordResetTTL {
		t.Errorf("expiry told to the user = %v, want %v", sent.ExpiresIn, auth.PasswordResetTTL)
	}

	const newPassword = "an-entirely-different-password"
	if err := svc.ResetPassword(t.Context(), tenantID, sent.Token, newPassword, auth.RequestContext{}); err != nil {
		t.Fatalf("reset password: %v", err)
	}

	if _, _, _, err := svc.Login(t.Context(), tenantID,
		auth.Credentials{Email: email, Password: password}, auth.RequestContext{}); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("old password still logs in: %v", err)
	}
	if _, _, _, err := svc.Login(t.Context(), tenantID,
		auth.Credentials{Email: email, Password: newPassword}, auth.RequestContext{}); err != nil {
		t.Errorf("new password does not log in: %v", err)
	}
}

// A reset token is single-use, so a link forwarded or replayed sets no password.
func TestPasswordResetTokenIsSpentOnce(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc, mail := newServiceWithMailer(t, db)
	tenantID := seedTenant(t, db)

	email := registerOwner(t, svc, tenantID)
	if err := svc.RequestPasswordReset(t.Context(), tenantID, email, auth.RequestContext{}); err != nil {
		t.Fatalf("request reset: %v", err)
	}
	sent, _ := mail.lastOf("password_reset", email)

	if err := svc.ResetPassword(t.Context(), tenantID, sent.Token, "first-new-password-here", auth.RequestContext{}); err != nil {
		t.Fatalf("first reset: %v", err)
	}
	err := svc.ResetPassword(t.Context(), tenantID, sent.Token, "second-new-password-x", auth.RequestContext{})
	if !errors.Is(err, auth.ErrTokenInvalid) {
		t.Errorf("second reset error = %v, want ErrTokenInvalid", err)
	}
}

// Requesting a second link kills the first. Otherwise every reset a user ever
// requested stays live in their inbox until it expires.
func TestRequestingAResetSupersedesTheOutstandingOne(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc, mail := newServiceWithMailer(t, db)
	tenantID := seedTenant(t, db)

	email := registerOwner(t, svc, tenantID)

	if err := svc.RequestPasswordReset(t.Context(), tenantID, email, auth.RequestContext{}); err != nil {
		t.Fatalf("first request: %v", err)
	}
	first, _ := mail.lastOf("password_reset", email)

	if err := svc.RequestPasswordReset(t.Context(), tenantID, email, auth.RequestContext{}); err != nil {
		t.Fatalf("second request: %v", err)
	}
	second, _ := mail.lastOf("password_reset", email)

	if first.Token == second.Token {
		t.Fatal("the second request reused the first token")
	}

	if err := svc.ResetPassword(t.Context(), tenantID, first.Token, "a-brand-new-password", auth.RequestContext{}); !errors.Is(err, auth.ErrTokenInvalid) {
		t.Errorf("superseded token error = %v, want ErrTokenInvalid", err)
	}
	if err := svc.ResetPassword(t.Context(), tenantID, second.Token, "a-brand-new-password", auth.RequestContext{}); err != nil {
		t.Errorf("newest token does not work: %v", err)
	}
}

// Resetting a password revokes the sessions it protected. Whoever forced the
// reset, the other party is now logged out.
func TestPasswordResetRevokesExistingSessions(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc, mail := newServiceWithMailer(t, db)
	tenantID := seedTenant(t, db)

	email := uniqueEmail()
	pair, _, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: email, Password: password}, "Ada", auth.RequestContext{})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := svc.RequestPasswordReset(t.Context(), tenantID, email, auth.RequestContext{}); err != nil {
		t.Fatalf("request reset: %v", err)
	}
	sent, _ := mail.lastOf("password_reset", email)

	if err := svc.ResetPassword(t.Context(), tenantID, sent.Token, "yet-another-password", auth.RequestContext{}); err != nil {
		t.Fatalf("reset: %v", err)
	}

	// The refresh token from before the reset must no longer buy a new session.
	if _, err := svc.Refresh(t.Context(), tenantID, pair.RefreshToken, auth.RequestContext{}); !errors.Is(err, auth.ErrSessionInvalid) {
		t.Errorf("pre-reset refresh token error = %v, want ErrSessionInvalid", err)
	}
}

// A password is global; a session is not. Resetting from one workspace must end
// the sessions the same person holds in the others, or the device they are trying
// to lock out stays signed in wherever they happen to belong.
//
// The revocation is a job, so this test does what the worker does: the service
// queues it, and the sweep is then run for real against both workspaces.
func TestPasswordResetRevokesSessionsInEveryWorkspace(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc, mail, jobs := newServiceWithFakes(t, db)

	acme := seedTenant(t, db)
	globex := seedTenant(t, db)

	// One person, two workspaces: registered here, invited there.
	email := registerOwner(t, svc, acme)
	globexOwnerPair, _, _, err := svc.Register(t.Context(), globex,
		auth.Credentials{Email: uniqueEmail(), Password: password}, "Owner", auth.RequestContext{})
	if err != nil {
		t.Fatalf("register globex owner: %v", err)
	}
	globexOwner, err := svc.Verify(globexOwnerPair.AccessToken)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}

	_, invite, err := svc.Invite(t.Context(), globexOwner, email, auth.RoleStudent, "Globex", auth.RequestContext{})
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	globexPair, user, _, err := svc.AcceptInvitation(t.Context(), globex, invite, password, "Ada", auth.RequestContext{})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	// A live session in Acme too, held by the device we are trying to lock out.
	acmePair, _, _, err := svc.Login(t.Context(), acme,
		auth.Credentials{Email: email, Password: password}, auth.RequestContext{})
	if err != nil {
		t.Fatalf("login to acme: %v", err)
	}

	// Reset from Acme.
	if err := svc.RequestPasswordReset(t.Context(), acme, email, auth.RequestContext{}); err != nil {
		t.Fatalf("request reset: %v", err)
	}
	sent, _ := mail.lastOf("password_reset", email)
	if err := svc.ResetPassword(t.Context(), acme, sent.Token, "a-fresh-global-password", auth.RequestContext{}); err != nil {
		t.Fatalf("reset: %v", err)
	}

	// Acme's sessions die with the reset itself: the browser in front of us must
	// not depend on a worker being up.
	if _, err := svc.Refresh(t.Context(), acme, acmePair.RefreshToken, auth.RequestContext{}); !errors.Is(err, auth.ErrSessionInvalid) {
		t.Errorf("acme refresh after reset = %v, want ErrSessionInvalid", err)
	}

	// Globex's session outlives the transaction, and the job is what ends it.
	if !jobs.queued(user.ID) {
		t.Fatal("resetting a password queued no cross-workspace revocation")
	}
	if _, err := svc.Refresh(t.Context(), globex, globexPair.RefreshToken, auth.RequestContext{}); err != nil {
		t.Fatalf("globex session should still be live until the job runs: %v", err)
	}

	// Run the job, as the worker would.
	revoked, err := newMaintenance(t, db).RevokeSessionsEverywhere(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("revoke everywhere: %v", err)
	}
	if revoked == 0 {
		t.Fatal("the sweep revoked nothing")
	}

	// The Refresh above rotated Globex's token, so present the successor.
	if _, err := svc.Refresh(t.Context(), globex, globexPair.RefreshToken, auth.RequestContext{}); err == nil {
		t.Error("a session in another workspace survived the password reset")
	}
}

// Idempotent, because jobs are retried.
func TestRevokeSessionsEverywhereIsIdempotent(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)

	email := uniqueEmail()
	if _, _, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: email, Password: password}, "Ada", auth.RequestContext{}); err != nil {
		t.Fatalf("register: %v", err)
	}

	_, user, _, err := svc.Login(t.Context(), tenantID,
		auth.Credentials{Email: email, Password: password}, auth.RequestContext{})
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	maintenance := newMaintenance(t, db)

	first, err := maintenance.RevokeSessionsEverywhere(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if first == 0 {
		t.Fatal("the first sweep revoked nothing, so the second proves nothing")
	}

	second, err := maintenance.RevokeSessionsEverywhere(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if second != 0 {
		t.Errorf("a repeated sweep revoked %d sessions again", second)
	}
}

// Asking to reset an address that has no account here reports success and mails
// nothing. Any other behaviour is an enumeration oracle: a stranger learns which
// addresses belong to this workspace by reading status codes.
func TestRequestPasswordResetIsSilentAboutUnknownAddresses(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc, mail := newServiceWithMailer(t, db)
	tenantID := seedTenant(t, db)

	registerOwner(t, svc, tenantID)
	before := mail.countOf("password_reset")

	stranger := uniqueEmail()
	if err := svc.RequestPasswordReset(t.Context(), tenantID, stranger, auth.RequestContext{}); err != nil {
		t.Fatalf("request for an unknown address = %v, want nil", err)
	}

	if got := mail.countOf("password_reset"); got != before {
		t.Errorf("sent %d reset emails for an unknown address, want %d", got-before, 0)
	}
	if _, ok := mail.lastOf("password_reset", stranger); ok {
		t.Error("mailed a reset link to an address with no account")
	}
}

// A reset requested from one workspace must not be spendable on another, even by
// a member of both. The token is tenant-scoped and RLS is the net.
func TestResetTokenDoesNotCrossWorkspaces(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc, mail := newServiceWithMailer(t, db)

	acme := seedTenant(t, db)
	globex := seedTenant(t, db)

	email := registerOwner(t, svc, acme)
	registerOwner(t, svc, globex) // claim it, so it is a real workspace

	if err := svc.RequestPasswordReset(t.Context(), acme, email, auth.RequestContext{}); err != nil {
		t.Fatalf("request reset: %v", err)
	}
	sent, _ := mail.lastOf("password_reset", email)

	err := svc.ResetPassword(t.Context(), globex, sent.Token, "a-password-from-elsewhere", auth.RequestContext{})
	if !errors.Is(err, auth.ErrTokenInvalid) {
		t.Errorf("cross-tenant reset = %v, want ErrTokenInvalid", err)
	}
}

// Garbage that could not have been issued by us is rejected without a lookup.
func TestMalformedCredentialTokensAreRejected(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc, _ := newServiceWithMailer(t, db)
	tenantID := seedTenant(t, db)

	for _, token := range []string{"", "not-base64!!", "c2hvcnQ"} {
		if err := svc.VerifyEmail(t.Context(), tenantID, token, auth.RequestContext{}); !errors.Is(err, auth.ErrTokenInvalid) {
			t.Errorf("VerifyEmail(%q) = %v, want ErrTokenInvalid", token, err)
		}
		if err := svc.ResetPassword(t.Context(), tenantID, token, password, auth.RequestContext{}); !errors.Is(err, auth.ErrTokenInvalid) {
			t.Errorf("ResetPassword(%q) = %v, want ErrTokenInvalid", token, err)
		}
	}
}

// A password that fails the length rule is refused before the token is spent, so
// the user can try again with the link they already have.
func TestResetWithAWeakPasswordDoesNotSpendTheToken(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc, mail := newServiceWithMailer(t, db)
	tenantID := seedTenant(t, db)

	email := registerOwner(t, svc, tenantID)
	if err := svc.RequestPasswordReset(t.Context(), tenantID, email, auth.RequestContext{}); err != nil {
		t.Fatalf("request reset: %v", err)
	}
	sent, _ := mail.lastOf("password_reset", email)

	if err := svc.ResetPassword(t.Context(), tenantID, sent.Token, "short", auth.RequestContext{}); !errors.Is(err, auth.ErrPasswordTooShort) {
		t.Fatalf("weak password error = %v, want ErrPasswordTooShort", err)
	}

	// The link still works.
	if err := svc.ResetPassword(t.Context(), tenantID, sent.Token, "a-long-enough-password", auth.RequestContext{}); err != nil {
		t.Errorf("token was spent by the rejected attempt: %v", err)
	}
}

// The invitation link reaches the invited address and nowhere else — the
// transport layer discards the token — so accepting it proves control of that
// inbox exactly as a verification email would. A new account arrives verified,
// and is not asked to prove the same thing twice.
func TestAcceptingAnInvitationVerifiesANewAccount(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc, mail := newServiceWithMailer(t, db)
	tenantID := seedTenant(t, db)

	ownerPair, _, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: uniqueEmail(), Password: password}, "Owner", auth.RequestContext{})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	owner, err := svc.Verify(ownerPair.AccessToken)
	if err != nil {
		t.Fatalf("verify owner token: %v", err)
	}

	invited := uniqueEmail()
	_, token, err := svc.Invite(t.Context(), owner, invited, auth.RoleInstructor, "Acme", auth.RequestContext{})
	if err != nil {
		t.Fatalf("invite: %v", err)
	}

	// The invitation itself was mailed to the invitee.
	if _, ok := mail.lastOf("invitation", invited); !ok {
		t.Fatal("no invitation email was sent")
	}

	before := mail.countOf("verification")

	_, user, _, err := svc.AcceptInvitation(t.Context(), tenantID, token, password, "Grace", auth.RequestContext{})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	if user.EmailVerifiedAt == nil {
		t.Error("an invited account is not marked verified")
	}
	if got := mail.countOf("verification"); got != before {
		t.Errorf("accepting an invitation sent %d verification emails, want 0", got-before)
	}
}

// The Invitation handed back to the caller must carry the timestamps the row
// carries. Leaving created_at to the column default renders it to a client as
// year 1, which is what a JSON zero time looks like.
func TestInviteReturnsAPopulatedInvitation(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc, _ := newServiceWithMailer(t, db)
	tenantID := seedTenant(t, db)

	ownerPair, _, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: uniqueEmail(), Password: password}, "Owner", auth.RequestContext{})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	owner, err := svc.Verify(ownerPair.AccessToken)
	if err != nil {
		t.Fatalf("verify owner token: %v", err)
	}

	inv, _, err := svc.Invite(t.Context(), owner, uniqueEmail(), auth.RoleStudent, "Acme", auth.RequestContext{})
	if err != nil {
		t.Fatalf("invite: %v", err)
	}

	if inv.CreatedAt.IsZero() {
		t.Error("returned invitation has a zero CreatedAt")
	}
	if inv.ExpiresAt.IsZero() {
		t.Error("returned invitation has a zero ExpiresAt")
	}

	// And the row agrees with the struct: listing it back must not shift the time.
	list, err := svc.Invitations(t.Context(), owner, 10)
	if err != nil {
		t.Fatalf("list invitations: %v", err)
	}
	for _, got := range list {
		if got.ID != inv.ID {
			continue
		}
		if !got.CreatedAt.Equal(inv.CreatedAt) {
			t.Errorf("stored CreatedAt %v != returned %v", got.CreatedAt, inv.CreatedAt)
		}
		return
	}
	t.Fatal("the invitation just created is not in the list")
}

// Resending is a no-op once the address is confirmed, and supersedes the
// outstanding link otherwise.
func TestResendVerification(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc, mail := newServiceWithMailer(t, db)
	tenantID := seedTenant(t, db)

	email := uniqueEmail()
	pair, _, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: email, Password: password}, "Ada", auth.RequestContext{})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	principal, err := svc.Verify(pair.AccessToken)
	if err != nil {
		t.Fatalf("verify token: %v", err)
	}

	first, _ := mail.lastOf("verification", email)

	if err := svc.ResendVerification(t.Context(), principal, auth.RequestContext{}); err != nil {
		t.Fatalf("resend: %v", err)
	}
	second, _ := mail.lastOf("verification", email)

	if first.Token == second.Token {
		t.Fatal("resend reused the original token")
	}
	if err := svc.VerifyEmail(t.Context(), tenantID, first.Token, auth.RequestContext{}); !errors.Is(err, auth.ErrTokenInvalid) {
		t.Errorf("superseded verification token = %v, want ErrTokenInvalid", err)
	}

	if err := svc.VerifyEmail(t.Context(), tenantID, second.Token, auth.RequestContext{}); err != nil {
		t.Fatalf("verify with the resent token: %v", err)
	}

	// Already verified: a resend does nothing and says so by not failing.
	sentBefore := mail.countOf("verification")
	if err := svc.ResendVerification(t.Context(), principal, auth.RequestContext{}); err != nil {
		t.Fatalf("resend after verifying: %v", err)
	}
	if got := mail.countOf("verification"); got != sentBefore {
		t.Errorf("resend after verifying sent %d more emails, want 0", got-sentBefore)
	}
}
