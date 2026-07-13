package auth_test

import (
	"errors"
	"testing"

	"github.com/ebnsina/muallim-api/internal/auth"
)

func TestRenamingYourself(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)

	tokens, _, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: uniqueEmail(), Password: password}, "First", auth.RequestContext{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	p, err := svc.Verify(tokens.AccessToken)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	user, err := svc.Rename(t.Context(), p, "  Ibn al-Haytham  ", auth.RequestContext{})
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}

	// Trimmed, because a name with a space on each end is a name somebody typed badly.
	if user.Name != "Ibn al-Haytham" {
		t.Errorf("name = %q, want %q", user.Name, "Ibn al-Haytham")
	}
}

func TestANameCannotBeNothing(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)

	tokens, _, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: uniqueEmail(), Password: password}, "First", auth.RequestContext{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	p, err := svc.Verify(tokens.AccessToken)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if _, err := svc.Rename(t.Context(), p, "   ", auth.RequestContext{}); !errors.Is(err, auth.ErrNameInvalid) {
		t.Fatalf("a name of spaces returned %v, want ErrNameInvalid", err)
	}
}

/*
The one that matters: a token is not enough to change a password.

Otherwise a stolen access token — the cheapest thing to steal — is enough to lock
somebody out of their own account for good, which is the difference between an
intrusion and a loss.
*/
func TestChangingAPasswordWithoutKnowingItIsRefused(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)

	email := uniqueEmail()
	tokens, _, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: email, Password: password}, "First", auth.RequestContext{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	p, err := svc.Verify(tokens.AccessToken)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	err = svc.ChangePassword(t.Context(), p, "not-the-password", "a-brand-new-password-1", auth.RequestContext{})
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("ChangePassword with the wrong current password returned %v, want ErrInvalidCredentials", err)
	}

	// And the old password still works, which is the other half of the promise.
	if _, _, _, err := svc.Login(t.Context(), tenantID,
		auth.Credentials{Email: email, Password: password}, auth.RequestContext{}); err != nil {
		t.Fatalf("the old password stopped working after a refused change: %v", err)
	}
}

func TestChangingAPasswordEndsEverySessionButThisOne(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)

	email := uniqueEmail()
	first, _, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: email, Password: password}, "First", auth.RequestContext{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// A second browser, signed in as the same person. This is the one that must die.
	second, _, _, err := svc.Login(t.Context(), tenantID,
		auth.Credentials{Email: email, Password: password}, auth.RequestContext{})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	p, err := svc.Verify(second.AccessToken)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	const next = "a-brand-new-password-1"
	if err := svc.ChangePassword(t.Context(), p, password, next, auth.RequestContext{}); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	// The session that asked keeps working.
	if _, err := svc.Refresh(t.Context(), tenantID, second.RefreshToken, auth.RequestContext{}); err != nil {
		t.Errorf("the browser that changed the password was signed out: %v", err)
	}

	// The other one does not.
	if _, err := svc.Refresh(t.Context(), tenantID, first.RefreshToken, auth.RequestContext{}); !errors.Is(err, auth.ErrSessionInvalid) {
		t.Errorf("another session survived a password change: %v", err)
	}

	// And the new password is the password.
	if _, _, _, err := svc.Login(t.Context(), tenantID,
		auth.Credentials{Email: email, Password: next}, auth.RequestContext{}); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
}
