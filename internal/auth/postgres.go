package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// uniqueViolation is the SQLSTATE Postgres raises for a unique index conflict.
const uniqueViolation = "23505"

// PostgresRepository satisfies Repository. Every method takes the pgx.Tx handed
// to it by database.WithTenant, so no query escapes the tenant binding.
type PostgresRepository struct{}

// NewPostgresRepository returns a Repository backed by Postgres.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

// CreateUser inserts a user with an application-generated id.
//
// The id is generated in Go rather than by RETURNING because the users table has
// a SELECT policy requiring a membership, and RETURNING applies SELECT policies —
// so RETURNING would fail on a row whose membership does not exist yet.
func (r *PostgresRepository) CreateUser(ctx context.Context, tx pgx.Tx, u User, passwordHash string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO users (id, email, password_hash, name) VALUES ($1, $2, $3, $4)`,
		u.ID, u.Email, passwordHash, u.Name)

	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return ErrEmailTaken
		}
		return fmt.Errorf("auth: create user: %w", err)
	}
	return nil
}

// CreateMembership binds a user to the bound tenant.
func (r *PostgresRepository) CreateMembership(ctx context.Context, tx pgx.Tx, m Membership) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO memberships (id, tenant_id, user_id, role) VALUES ($1, $2, $3, $4)`,
		m.ID, m.TenantID, m.UserID, m.Role)
	if err != nil {
		return fmt.Errorf("auth: create membership: %w", err)
	}
	return nil
}

// credentialsByEmailSQL joins the membership so a user with no membership on the
// bound tenant is simply not found — the same outcome as a missing account.
//
// The users SELECT policy would hide the row anyway; the join makes the intent
// explicit rather than relying on the net.
const credentialsByEmailSQL = `
	SELECT u.id, u.email, u.name, u.email_verified_at, u.created_at,
	       u.password_hash, m.id, m.role, m.status
	FROM users u
	JOIN memberships m ON m.user_id = u.id AND m.tenant_id = $1
	WHERE lower(u.email) = lower($2)`

// CredentialsByEmail loads the user, their password hash, and their membership.
func (r *PostgresRepository) CredentialsByEmail(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, email string) (User, Membership, string, error) {
	var (
		u    User
		m    Membership
		hash string
	)

	err := tx.QueryRow(ctx, credentialsByEmailSQL, tenantID, email).Scan(
		&u.ID, &u.Email, &u.Name, &u.EmailVerifiedAt, &u.CreatedAt,
		&hash, &m.ID, &m.Role, &m.Status)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, Membership{}, "", ErrInvalidCredentials
		}
		return User{}, Membership{}, "", fmt.Errorf("auth: load credentials: %w", err)
	}

	m.TenantID = tenantID
	m.UserID = u.ID
	return u, m, hash, nil
}

const userByIDSQL = `
	SELECT u.id, u.email, u.name, u.email_verified_at, u.created_at, m.role
	FROM users u
	JOIN memberships m ON m.user_id = u.id AND m.tenant_id = $1
	WHERE u.id = $2 AND m.status = 'active'`

// UserByID loads a user and their role on the bound tenant.
func (r *PostgresRepository) UserByID(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) (User, string, error) {
	var (
		u    User
		role string
	)
	err := tx.QueryRow(ctx, userByIDSQL, tenantID, userID).Scan(
		&u.ID, &u.Email, &u.Name, &u.EmailVerifiedAt, &u.CreatedAt, &role)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, "", ErrInvalidCredentials
		}
		return User{}, "", fmt.Errorf("auth: load user: %w", err)
	}
	return u, role, nil
}

// Session is a stored refresh token.
type Session struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	UserID    uuid.UUID
	FamilyID  uuid.UUID
	ExpiresAt time.Time
	RevokedAt *time.Time

	// ReplacedBy is set when this token was rotated away by a successful refresh.
	// Presenting such a token again is evidence of theft. A token that is merely
	// revoked — by logout, or as collateral when its family was swept — has no
	// successor, and is simply invalid.
	ReplacedBy *uuid.UUID
}

// Rotated reports whether this token was already exchanged for a successor.
func (s Session) Rotated() bool { return s.ReplacedBy != nil }

// Live reports whether the session may still be exchanged.
func (s Session) Live(now time.Time) bool {
	return s.RevokedAt == nil && now.Before(s.ExpiresAt)
}

// CreateSession stores a refresh token digest.
func (r *PostgresRepository) CreateSession(ctx context.Context, tx pgx.Tx, s Session, digest []byte, rc RequestContext) error {
	var ip *string
	if rc.IP.IsValid() {
		v := rc.IP.String()
		ip = &v
	}

	_, err := tx.Exec(ctx,
		`INSERT INTO sessions (id, tenant_id, user_id, family_id, token_hash, expires_at, user_agent, ip)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		s.ID, s.TenantID, s.UserID, s.FamilyID, digest, s.ExpiresAt, rc.UserAgent, ip)
	if err != nil {
		return fmt.Errorf("auth: create session: %w", err)
	}
	return nil
}

const sessionByDigestSQL = `
	SELECT id, tenant_id, user_id, family_id, expires_at, revoked_at, replaced_by
	FROM sessions
	WHERE tenant_id = $1 AND token_hash = $2`

// SessionByDigest looks up a session by the digest of its refresh token.
//
// A revoked or expired session is returned rather than hidden: the service must
// distinguish "this token never existed" from "this token was already rotated
// away", because the second is evidence of theft.
func (r *PostgresRepository) SessionByDigest(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, digest []byte) (Session, error) {
	var s Session
	err := tx.QueryRow(ctx, sessionByDigestSQL, tenantID, digest).Scan(
		&s.ID, &s.TenantID, &s.UserID, &s.FamilyID, &s.ExpiresAt, &s.RevokedAt, &s.ReplacedBy)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrSessionInvalid
		}
		return Session{}, fmt.Errorf("auth: load session: %w", err)
	}
	return s, nil
}

// RotateSession revokes the old session and points it at its successor, so a
// later presentation of the old token is recognisable as a replay.
//
// The successor row must already exist: replaced_by is a foreign key. Callers
// therefore insert the new session first, within the same transaction, so the
// pair is atomic either way.
func (r *PostgresRepository) RotateSession(ctx context.Context, tx pgx.Tx, tenantID, oldID, newID uuid.UUID) error {
	_, err := tx.Exec(ctx,
		`UPDATE sessions SET revoked_at = now(), replaced_by = $3
		 WHERE tenant_id = $1 AND id = $2 AND revoked_at IS NULL`,
		tenantID, oldID, newID)
	if err != nil {
		return fmt.Errorf("auth: rotate session: %w", err)
	}
	return nil
}

// RevokeFamily revokes every live session descended from one login.
//
// Called when a rotated-away token is presented again: either an attacker stole
// it, or the legitimate client replayed it. We cannot tell which, so both are
// logged out and the user re-authenticates.
func (r *PostgresRepository) RevokeFamily(ctx context.Context, tx pgx.Tx, tenantID, familyID uuid.UUID) (int64, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE sessions SET revoked_at = now()
		 WHERE tenant_id = $1 AND family_id = $2 AND revoked_at IS NULL`,
		tenantID, familyID)
	if err != nil {
		return 0, fmt.Errorf("auth: revoke family: %w", err)
	}
	return tag.RowsAffected(), nil
}

// RevokeSession revokes one session. Logging out must not log a user out of
// their other devices.
func (r *PostgresRepository) RevokeSession(ctx context.Context, tx pgx.Tx, tenantID, sessionID uuid.UUID) error {
	_, err := tx.Exec(ctx,
		`UPDATE sessions SET revoked_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND revoked_at IS NULL`,
		tenantID, sessionID)
	if err != nil {
		return fmt.Errorf("auth: revoke session: %w", err)
	}
	return nil
}
