package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CreateToken stores the digest of a credential token. The token itself is never
// written anywhere.
func (r *PostgresRepository) CreateToken(ctx context.Context, tx pgx.Tx, t CredentialToken, digest []byte) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO user_tokens (id, tenant_id, user_id, purpose, token_hash, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		t.ID, t.TenantID, t.UserID, t.Purpose, digest, t.ExpiresAt)
	if err != nil {
		return fmt.Errorf("auth: create %s token: %w", t.Purpose, err)
	}
	return nil
}

// SupersedeTokens spends every outstanding token a user holds for one purpose, so
// that the link issued next is the only one that works.
func (r *PostgresRepository) SupersedeTokens(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, purpose string) error {
	_, err := tx.Exec(ctx,
		`UPDATE user_tokens SET consumed_at = now()
		 WHERE tenant_id = $1 AND user_id = $2 AND purpose = $3 AND consumed_at IS NULL`,
		tenantID, userID, purpose)
	if err != nil {
		return fmt.Errorf("auth: supersede %s tokens: %w", purpose, err)
	}
	return nil
}

const tokenByDigestSQL = `
	SELECT id, tenant_id, user_id, purpose, expires_at, consumed_at, created_at
	FROM user_tokens
	WHERE tenant_id = $1 AND purpose = $2 AND token_hash = $3`

// TokenByDigest loads a token by its digest.
//
// The purpose is part of the lookup, not a property checked afterwards: a
// verification token must never be presentable as a reset token, and the cheapest
// way to guarantee that is for the query to not find it.
func (r *PostgresRepository) TokenByDigest(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, purpose string, digest []byte) (CredentialToken, error) {
	var t CredentialToken

	err := tx.QueryRow(ctx, tokenByDigestSQL, tenantID, purpose, digest).Scan(
		&t.ID, &t.TenantID, &t.UserID, &t.Purpose, &t.ExpiresAt, &t.ConsumedAt, &t.CreatedAt)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CredentialToken{}, ErrTokenInvalid
		}
		return CredentialToken{}, fmt.Errorf("auth: load %s token: %w", purpose, err)
	}
	return t, nil
}

// ConsumeToken spends a token, reporting whether this call is the one that spent
// it. A second caller matches zero rows and is told so.
func (r *PostgresRepository) ConsumeToken(ctx context.Context, tx pgx.Tx, tenantID, tokenID uuid.UUID) (bool, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE user_tokens SET consumed_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND consumed_at IS NULL`,
		tenantID, tokenID)
	if err != nil {
		return false, fmt.Errorf("auth: consume token: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// SetPasswordHash replaces a user's password.
//
// users is global and carries no tenant_id, so the membership is named
// explicitly rather than left to the RLS policy that says the same thing. A
// missing row means the user is not a member here, and that is a caller bug
// rather than an ordinary outcome: the token that authorised this was minted
// against this workspace.
func (r *PostgresRepository) SetPasswordHash(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, hash string) error {
	tag, err := tx.Exec(ctx,
		`UPDATE users SET password_hash = $1, updated_at = now()
		 WHERE id = $2
		   AND EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = users.id AND m.tenant_id = $3)`,
		hash, userID, tenantID)
	if err != nil {
		return fmt.Errorf("auth: set password hash: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("auth: set password hash: user %s is not a member of tenant %s", userID, tenantID)
	}
	return nil
}

// MarkEmailVerified records that a user proved control of their address.
func (r *PostgresRepository) MarkEmailVerified(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, at time.Time) error {
	tag, err := tx.Exec(ctx,
		`UPDATE users SET email_verified_at = $1, updated_at = now()
		 WHERE id = $2
		   AND EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = users.id AND m.tenant_id = $3)`,
		at, userID, tenantID)
	if err != nil {
		return fmt.Errorf("auth: mark email verified: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("auth: mark email verified: user %s is not a member of tenant %s", userID, tenantID)
	}
	return nil
}
