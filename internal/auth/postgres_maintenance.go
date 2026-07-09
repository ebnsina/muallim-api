package auth

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// orphanedUsersSQL selects users who belong to no workspace.
//
// It carries no WHERE clause of its own. The users_orphan_visible policy already
// restricts an unbound session to exactly the orphans, and restating the
// predicate here would invite the two to drift — with the policy, which is the
// one that actually decides, drifting silently.
//
// Ordered by created_at so the oldest account waiting to be erased is erased
// first, and so a batch is a stable prefix rather than whatever the heap offers.
const orphanedUsersSQL = `
	SELECT id, email, name, email_verified_at, created_at
	FROM users
	ORDER BY created_at, id
	LIMIT $1`

// OrphanedUsers lists users with no membership in any workspace.
//
// It must run under database.WithoutTenant. Under a bound tenant the policy
// yields nothing, which would look like "no orphans" rather than like a mistake.
func (r *PostgresRepository) OrphanedUsers(ctx context.Context, tx pgx.Tx, limit int) ([]User, error) {
	rows, err := tx.Query(ctx, orphanedUsersSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("auth: list orphaned users: %w", err)
	}
	defer rows.Close()

	users, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (User, error) {
		var u User
		err := row.Scan(&u.ID, &u.Email, &u.Name, &u.EmailVerifiedAt, &u.CreatedAt)
		return u, err
	})
	if err != nil {
		return nil, fmt.Errorf("auth: scan orphaned users: %w", err)
	}
	return users, nil
}

// DeleteUser erases a user, reporting whether a row was removed.
//
// Zero rows is not an error. An unbound session can only see orphans, and
// Postgres applies SELECT policies to the rows a DELETE has to read, so a user
// who joined a workspace since being listed is invisible to this statement and
// simply not deleted.
//
// Sessions, enrolments, progress, and credential tokens go with them by foreign
// key — referential actions are not subject to row-level security, which is the
// only reason a cascade can reach rows this session cannot see. Audit entries
// keep their rows and lose their actor: a record of what happened must outlive
// the person it happened to, and must not name them.
func (r *PostgresRepository) DeleteUser(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (bool, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	if err != nil {
		return false, fmt.Errorf("auth: delete user %s: %w", userID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// RevokeSessionsEverywhere ends every live session a user holds, in every
// workspace, and reports how many it ended.
//
// No tenant_id in the WHERE clause, deliberately: that is the whole point, and
// it is why this must run under database.WithoutTenant. Under a bound tenant the
// sessions of every other workspace are invisible and the statement would report
// success having changed nothing.
func (r *PostgresRepository) RevokeSessionsEverywhere(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (int64, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE sessions SET revoked_at = now()
		 WHERE user_id = $1 AND revoked_at IS NULL`,
		userID)
	if err != nil {
		return 0, fmt.Errorf("auth: revoke sessions everywhere for %s: %w", userID, err)
	}
	return tag.RowsAffected(), nil
}
