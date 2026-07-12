package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const testSecret = "a-signing-secret-of-at-least-32-bytes"

func testIssuer(t *testing.T) *TokenIssuer {
	t.Helper()
	issuer, err := NewTokenIssuer(testSecret, "muallim-api-test")
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}
	return issuer
}

func testPrincipal() Principal {
	return Principal{
		UserID:    uuid.New(),
		TenantID:  uuid.New(),
		SessionID: uuid.New(),
		Role:      RoleInstructor,
	}
}

// A short key is recoverable offline, which makes every other control in this
// package irrelevant.
func TestNewTokenIssuerRejectsShortSecrets(t *testing.T) {
	t.Parallel()

	if _, err := NewTokenIssuer(strings.Repeat("x", minSecretLen-1), "iss"); err == nil {
		t.Error("a signing secret below the minimum length was accepted")
	}
	if _, err := NewTokenIssuer(strings.Repeat("x", minSecretLen), "iss"); err != nil {
		t.Errorf("a secret of exactly the minimum length was rejected: %v", err)
	}
}

func TestIssueThenVerifyRoundTrips(t *testing.T) {
	t.Parallel()

	issuer := testIssuer(t)
	want := testPrincipal()

	raw, expiresAt, err := issuer.Issue(want)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if time.Until(expiresAt) > AccessTokenTTL+time.Second {
		t.Errorf("token expires in %v, longer than the configured TTL", time.Until(expiresAt))
	}

	got, err := issuer.Verify(raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got != want {
		t.Errorf("round trip changed the principal:\n got %+v\nwant %+v", got, want)
	}
}

// The oldest JWT vulnerability: a token whose header claims "alg":"none" must
// never verify.
func TestVerifyRejectsAlgNone(t *testing.T) {
	t.Parallel()

	p := testPrincipal()
	unsigned := jwt.NewWithClaims(jwt.SigningMethodNone, claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   p.UserID.String(),
			Issuer:    "muallim-api-test",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		TenantID:  p.TenantID.String(),
		SessionID: p.SessionID.String(),
		Role:      p.Role,
	})

	raw, err := unsigned.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("could not construct the attack token: %v", err)
	}

	if _, err := testIssuer(t).Verify(raw); err == nil {
		t.Fatal("an unsigned token verified; the signing method is not pinned")
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	t.Parallel()

	forged, err := NewTokenIssuer("a-completely-different-secret-key-32", "muallim-api-test")
	if err != nil {
		t.Fatal(err)
	}

	raw, _, err := forged.Issue(testPrincipal())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := testIssuer(t).Verify(raw); err == nil {
		t.Fatal("a token signed with a different key verified")
	}
}

// A token minted by a sibling environment must not authenticate here.
func TestVerifyRejectsWrongIssuer(t *testing.T) {
	t.Parallel()

	other, err := NewTokenIssuer(testSecret, "some-other-service")
	if err != nil {
		t.Fatal(err)
	}

	raw, _, err := other.Issue(testPrincipal())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := testIssuer(t).Verify(raw); err == nil {
		t.Fatal("a token from a different issuer verified")
	}
}

func TestVerifyRejectsExpiredToken(t *testing.T) {
	t.Parallel()

	issuer := testIssuer(t)
	issuer.now = func() time.Time { return time.Now().Add(-2 * AccessTokenTTL) }

	raw, _, err := issuer.Issue(testPrincipal())
	if err != nil {
		t.Fatal(err)
	}

	issuer.now = time.Now
	if _, err := issuer.Verify(raw); err == nil {
		t.Fatal("an expired token verified")
	} else if !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("err = %v, want it to wrap ErrUnauthenticated", err)
	}
}

// A role we have since removed must deny, not silently grant nothing.
func TestVerifyRejectsUnknownRole(t *testing.T) {
	t.Parallel()

	issuer := testIssuer(t)
	p := testPrincipal()
	p.Role = "superuser"

	raw, _, err := issuer.Issue(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.Verify(raw); err == nil {
		t.Fatal("a token naming an unknown role verified")
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	t.Parallel()

	issuer := testIssuer(t)
	for _, raw := range []string{"", "not.a.token", "a.b", strings.Repeat("x", 500)} {
		if _, err := issuer.Verify(raw); err == nil {
			t.Errorf("garbage token %q verified", raw)
		}
	}
}

func TestRefreshTokensAreUniqueAndHashed(t *testing.T) {
	t.Parallel()

	first, firstDigest, err := newRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	second, secondDigest, err := newRefreshToken()
	if err != nil {
		t.Fatal(err)
	}

	if first == second {
		t.Fatal("two refresh tokens collided; the generator is not random")
	}
	if string(firstDigest) == string(secondDigest) {
		t.Fatal("two distinct tokens produced the same digest")
	}
	if strings.Contains(string(firstDigest), first) {
		t.Fatal("the digest contains the raw token")
	}
	if got := string(hashToken(first)); got != string(firstDigest) {
		t.Error("hashToken is not deterministic")
	}
}

func TestValidateRefreshTokenFormat(t *testing.T) {
	t.Parallel()

	valid, _, err := newRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateRefreshTokenFormat(valid); err != nil {
		t.Errorf("a freshly minted token was rejected: %v", err)
	}

	// Garbage must be rejected before it reaches the database.
	for _, bad := range []string{"", "short", "!!!not-base64!!!", strings.Repeat("A", 100)} {
		if err := validateRefreshTokenFormat(bad); err == nil {
			t.Errorf("malformed token %q passed format validation", bad)
		}
	}
}
