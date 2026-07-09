package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Token lifetimes.
//
// The access token is short because it cannot be revoked: it is verified by
// signature alone, with no database lookup, which is what makes it fast. Fifteen
// minutes bounds how long a revoked session keeps working.
//
// The refresh token is long-lived, opaque, stored, and therefore revocable.
const (
	AccessTokenTTL  = 15 * time.Minute
	RefreshTokenTTL = 30 * 24 * time.Hour

	// refreshTokenBytes gives 256 bits of entropy. The token is a bearer
	// credential with a month-long life; it is not guessable.
	refreshTokenBytes = 32

	// minSecretLen bounds the HMAC key. A short key makes the signature forgeable
	// offline, which makes every other control in this package irrelevant.
	minSecretLen = 32
)

// TokenIssuer mints and verifies access tokens.
type TokenIssuer struct {
	secret []byte
	issuer string
	now    func() time.Time
}

// NewTokenIssuer returns a TokenIssuer, refusing a secret too short to be safe.
func NewTokenIssuer(secret, issuer string) (*TokenIssuer, error) {
	if len(secret) < minSecretLen {
		return nil, fmt.Errorf("auth: signing secret must be at least %d bytes, got %d", minSecretLen, len(secret))
	}
	return &TokenIssuer{secret: []byte(secret), issuer: issuer, now: time.Now}, nil
}

// claims is the access token payload.
//
// The tenant is inside the signature. Without it, a token minted for one tenant
// would authenticate its bearer on every other tenant they happen to belong to —
// and on tenants they do not.
type claims struct {
	jwt.RegisteredClaims

	TenantID  string `json:"tid"`
	SessionID string `json:"sid"`
	Role      string `json:"rol"`
}

// Issue mints an access token for p.
func (t *TokenIssuer) Issue(p Principal) (string, time.Time, error) {
	now := t.now()
	expiresAt := now.Add(AccessTokenTTL)

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   p.UserID.String(),
			Issuer:    t.issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			ID:        uuid.NewString(),
		},
		TenantID:  p.TenantID.String(),
		SessionID: p.SessionID.String(),
		Role:      p.Role,
	})

	signed, err := token.SignedString(t.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: sign access token: %w", err)
	}
	return signed, expiresAt, nil
}

// Verify parses and validates an access token, returning the principal it
// asserts.
//
// The signing method is pinned. Without that check, a token whose header says
// "alg":"none" — or "HS256" when the server expects RS256 — verifies against
// nothing, which is the oldest JWT vulnerability there is.
func (t *TokenIssuer) Verify(raw string) (Principal, error) {
	parsed, err := jwt.ParseWithClaims(raw, &claims{},
		func(token *jwt.Token) (any, error) {
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("auth: unexpected signing method %v", token.Header["alg"])
			}
			return t.secret, nil
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(t.issuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	}

	c, ok := parsed.Claims.(*claims)
	if !ok || !parsed.Valid {
		return Principal{}, ErrUnauthenticated
	}

	userID, err := uuid.Parse(c.Subject)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	tenantID, err := uuid.Parse(c.TenantID)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	sessionID, err := uuid.Parse(c.SessionID)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	if _, known := rolePermissions[c.Role]; !known {
		// A role we have since removed. Deny rather than grant nothing silently.
		return Principal{}, ErrUnauthenticated
	}

	return Principal{
		UserID:    userID,
		TenantID:  tenantID,
		SessionID: sessionID,
		Role:      c.Role,
	}, nil
}

// newRefreshToken returns an opaque token and the digest to store.
//
// Only the digest is persisted. A refresh token is a bearer credential valid for
// a month; storing it in plaintext means a database dump is a month of sessions.
func newRefreshToken() (token string, digest []byte, err error) {
	buf := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("auth: generate refresh token: %w", err)
	}

	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, hashToken(token), nil
}

// hashToken digests a refresh token for lookup and storage.
//
// SHA-256, not Argon2: the token is 256 bits of uniform randomness, so there is
// no dictionary to attack and nothing for a slow hash to buy. A slow hash here
// would only make every refresh slow.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// ErrMalformedToken is returned when a refresh token could not have been issued
// by us, so no lookup is attempted.
var ErrMalformedToken = errors.New("auth: malformed refresh token")

// validateRefreshTokenFormat rejects garbage before it reaches the database.
func validateRefreshTokenFormat(token string) error {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != refreshTokenBytes {
		return ErrMalformedToken
	}
	return nil
}
