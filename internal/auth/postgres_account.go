package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SetName changes a person's display name.
func (r *PostgresRepository) SetName(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, name string) error {
	// The tenant predicate is defence in depth beside RLS, and matches the sibling
	// SetPasswordHash: RLS is the net, not the only control, and a statement that
	// takes a tenantID must use it rather than trust the net alone.
	tag, err := tx.Exec(ctx,
		`UPDATE users SET name = $2, updated_at = now()
		 WHERE id = $1
		   AND EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = users.id AND m.tenant_id = $3)`,
		userID, name, tenantID)
	if err != nil {
		return fmt.Errorf("auth: set name: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotMember
	}
	return nil
}

// PasswordHash is read to be checked against a password somebody just typed, and is
// returned to nothing else.
func (r *PostgresRepository) PasswordHash(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) (string, error) {
	var hash string
	err := tx.QueryRow(ctx,
		`SELECT password_hash FROM users
		 WHERE id = $1
		   AND EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = users.id AND m.tenant_id = $2)`,
		userID, tenantID).Scan(&hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotMember
		}
		return "", fmt.Errorf("auth: load password hash: %w", err)
	}
	return hash, nil
}

// RevokeOtherUserSessions ends every session this person has in this workspace
// except the one they are asking from.
func (r *PostgresRepository) RevokeOtherUserSessions(ctx context.Context, tx pgx.Tx, tenantID, userID, keep uuid.UUID) error {
	_, err := tx.Exec(ctx,
		`UPDATE sessions SET revoked_at = now()
		 WHERE tenant_id = $1 AND user_id = $2 AND id <> $3 AND revoked_at IS NULL`,
		tenantID, userID, keep)
	if err != nil {
		return fmt.Errorf("auth: revoke other sessions: %w", err)
	}
	return nil
}
