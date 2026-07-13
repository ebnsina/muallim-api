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

// CreateInvitation stores an invitation and its token digest.
//
// The partial unique index on (tenant_id, lower(email)) where the invitation is
// still pending makes a duplicate outstanding invitation a conflict rather than a
// second row. Re-inviting after expiry or acceptance is ordinary and permitted.
func (r *PostgresRepository) CreateInvitation(ctx context.Context, tx pgx.Tx, inv Invitation, digest []byte) error {
	// created_at is written rather than left to the column default, so the row and
	// the Invitation handed back to the caller carry the same instant. The default
	// would fill the column and leave the struct's zero value to be rendered as
	// year 1.
	_, err := tx.Exec(ctx,
		`INSERT INTO invitations (id, tenant_id, email, role, token_hash, invited_by, expires_at, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		inv.ID, inv.TenantID, inv.Email, inv.Role, digest, inv.InvitedBy, inv.ExpiresAt, inv.CreatedAt)

	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return ErrInvitationPending
		}
		return fmt.Errorf("auth: create invitation: %w", err)
	}
	return nil
}

const invitationByDigestSQL = `
	SELECT id, tenant_id, email, role, invited_by, expires_at, accepted_at, revoked_at, created_at
	FROM invitations
	WHERE tenant_id = $1 AND token_hash = $2`

// InvitationByDigest looks up an invitation by the digest of its token.
func (r *PostgresRepository) InvitationByDigest(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, digest []byte) (Invitation, error) {
	var inv Invitation
	err := tx.QueryRow(ctx, invitationByDigestSQL, tenantID, digest).Scan(
		&inv.ID, &inv.TenantID, &inv.Email, &inv.Role, &inv.InvitedBy,
		&inv.ExpiresAt, &inv.AcceptedAt, &inv.RevokedAt, &inv.CreatedAt)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Invitation{}, ErrInvitationInvalid
		}
		return Invitation{}, fmt.Errorf("auth: load invitation: %w", err)
	}
	return inv, nil
}

// MarkInvitationAccepted closes an invitation. The WHERE clause makes acceptance
// idempotent under concurrency: two simultaneous accepts, and only one updates a
// row.
func (r *PostgresRepository) MarkInvitationAccepted(ctx context.Context, tx pgx.Tx, tenantID, invitationID uuid.UUID) (bool, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE invitations SET accepted_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND accepted_at IS NULL AND revoked_at IS NULL`,
		tenantID, invitationID)
	if err != nil {
		return false, fmt.Errorf("auth: accept invitation: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RevokeInvitation withdraws an outstanding invitation.
func (r *PostgresRepository) RevokeInvitation(ctx context.Context, tx pgx.Tx, tenantID, invitationID uuid.UUID) (bool, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE invitations SET revoked_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND accepted_at IS NULL AND revoked_at IS NULL`,
		tenantID, invitationID)
	if err != nil {
		return false, fmt.Errorf("auth: revoke invitation: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Newest first, so the keyset runs the other way: `<` and not `>`.
const listInvitationsSQL = `
	SELECT id, tenant_id, email, role, invited_by, expires_at, accepted_at, revoked_at, created_at
	FROM invitations
	WHERE tenant_id = $1
	  AND ($2::timestamptz IS NULL OR (created_at, id) < ($2, $3))
	ORDER BY created_at DESC, id DESC
	LIMIT $4`

// ListInvitations returns a page of the workspace's invitations, newest first.
func (r *PostgresRepository) ListInvitations(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *cursor, limit int) ([]Invitation, error) {
	var (
		afterTime *time.Time
		afterID   *uuid.UUID
	)
	if after != nil {
		afterTime, afterID = &after.CreatedAt, &after.ID
	}

	rows, err := tx.Query(ctx, listInvitationsSQL, tenantID, afterTime, afterID, limit+1)
	if err != nil {
		return nil, fmt.Errorf("auth: list invitations: %w", err)
	}
	defer rows.Close()

	invitations, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Invitation, error) {
		var inv Invitation
		err := row.Scan(&inv.ID, &inv.TenantID, &inv.Email, &inv.Role, &inv.InvitedBy,
			&inv.ExpiresAt, &inv.AcceptedAt, &inv.RevokedAt, &inv.CreatedAt)
		return inv, err
	})
	if err != nil {
		return nil, fmt.Errorf("auth: scan invitations: %w", err)
	}
	return invitations, nil
}

// credentialsByEmailAnyMembershipSQL finds a global account by address without
// requiring a membership in the bound tenant.
//
// It is readable only because the users table has a policy making an invited
// address visible to the workspace that invited it. Outside an accept flow this
// query returns nothing, which is the point.
const credentialsByEmailAnyMembershipSQL = `
	SELECT id, email, name, email_verified_at, created_at, password_hash
	FROM users
	WHERE lower(email) = lower($1)`

// InvitedUserByEmail loads the global account for an invited address, if one
// exists.
func (r *PostgresRepository) InvitedUserByEmail(ctx context.Context, tx pgx.Tx, email string) (User, string, bool, error) {
	var (
		u    User
		hash string
	)
	err := tx.QueryRow(ctx, credentialsByEmailAnyMembershipSQL, email).Scan(
		&u.ID, &u.Email, &u.Name, &u.EmailVerifiedAt, &u.CreatedAt, &hash)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, "", false, nil
		}
		return User{}, "", false, fmt.Errorf("auth: load invited user: %w", err)
	}
	return u, hash, true, nil
}

// MembershipFor returns the caller's membership in the bound tenant.
func (r *PostgresRepository) MembershipFor(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) (Membership, error) {
	var m Membership
	err := tx.QueryRow(ctx,
		`SELECT id, tenant_id, user_id, role, status FROM memberships
		 WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID).
		Scan(&m.ID, &m.TenantID, &m.UserID, &m.Role, &m.Status)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Membership{}, ErrNotMember
		}
		return Membership{}, fmt.Errorf("auth: load membership: %w", err)
	}
	return m, nil
}

/*
The keyset. `(m.created_at, m.id) > ($2, $3)` is one comparison of one row
against one index, not `created_at > x OR (created_at = x AND id > y)` — which
Postgres cannot always use an index for, and which is the same thing written in a
way that scans.

`$2 IS NULL` is the first page. One statement, two shapes, and no string building.
*/
const listMembersSQL = `
	SELECT u.id, u.email, u.name, u.email_verified_at, u.created_at,
	       m.id, m.created_at, m.role, m.status
	FROM memberships m
	JOIN users u ON u.id = m.user_id
	WHERE m.tenant_id = $1
	  AND ($2::timestamptz IS NULL OR (m.created_at, m.id) > ($2, $3))
	ORDER BY m.created_at, m.id
	LIMIT $4`

// ListMembers returns a page of the workspace's members.
//
// One query, joined — not a membership query followed by a user query per row. It
// asks for limit+1 rows: the extra one is how the caller knows there is more,
// without a COUNT(*) that would scan the table to answer yes or no.
func (r *PostgresRepository) ListMembers(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *cursor, limit int) ([]Member, error) {
	var (
		afterTime *time.Time
		afterID   *uuid.UUID
	)
	if after != nil {
		afterTime, afterID = &after.CreatedAt, &after.ID
	}

	rows, err := tx.Query(ctx, listMembersSQL, tenantID, afterTime, afterID, limit+1)
	if err != nil {
		return nil, fmt.Errorf("auth: list members: %w", err)
	}
	defer rows.Close()

	members, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Member, error) {
		var m Member
		err := row.Scan(&m.User.ID, &m.User.Email, &m.User.Name,
			&m.User.EmailVerifiedAt, &m.User.CreatedAt,
			&m.MembershipID, &m.JoinedAt, &m.Role, &m.Status)
		return m, err
	})
	if err != nil {
		return nil, fmt.Errorf("auth: scan members: %w", err)
	}
	return members, nil
}

// CountOwners reports how many active owners the workspace has.
//
// Used to refuse the change that would leave a workspace with none. The row lock
// is taken by the caller's transaction, so two concurrent demotions cannot each
// observe two owners and each remove one.
func (r *PostgresRepository) CountOwners(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (int, error) {
	var n int
	err := tx.QueryRow(ctx,
		`SELECT count(*) FROM memberships
		 WHERE tenant_id = $1 AND role = 'owner' AND status = 'active'`, tenantID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("auth: count owners: %w", err)
	}
	return n, nil
}

// LockMemberships takes a transaction-scoped lock on the workspace's membership
// rows, so an owner count cannot go stale between the check and the write.
//
// A row-level FOR UPDATE on the memberships of one tenant. Cheap: the workspaces
// where it matters have tens of members, not millions.
func (r *PostgresRepository) LockMemberships(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	rows, err := tx.Query(ctx,
		`SELECT 1 FROM memberships WHERE tenant_id = $1 FOR UPDATE`, tenantID)
	if err != nil {
		return fmt.Errorf("auth: lock memberships: %w", err)
	}
	defer rows.Close()

	for rows.Next() { //nolint:revive // draining is the point; the lock is the result
	}
	return rows.Err()
}

// UpdateMemberRole changes a member's role.
func (r *PostgresRepository) UpdateMemberRole(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, role string) error {
	tag, err := tx.Exec(ctx,
		`UPDATE memberships SET role = $3, updated_at = now()
		 WHERE tenant_id = $1 AND user_id = $2`,
		tenantID, userID, role)
	if err != nil {
		return fmt.Errorf("auth: update member role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotMember
	}
	return nil
}

// RemoveMember deletes a membership.
//
// The user account itself survives: it is global, and other workspaces may still
// hold memberships against it. Removing someone from a workspace is not erasing
// them from the platform.
func (r *PostgresRepository) RemoveMember(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) error {
	tag, err := tx.Exec(ctx,
		`DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID)
	if err != nil {
		return fmt.Errorf("auth: remove member: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotMember
	}
	return nil
}

// RevokeAllUserSessions ends every session a user holds in this workspace. Called
// when they are removed or demoted: an access token outlives the change that
// invalidated it, and a refresh token would mint a new one.
func (r *PostgresRepository) RevokeAllUserSessions(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) error {
	_, err := tx.Exec(ctx,
		`UPDATE sessions SET revoked_at = now()
		 WHERE tenant_id = $1 AND user_id = $2 AND revoked_at IS NULL`,
		tenantID, userID)
	if err != nil {
		return fmt.Errorf("auth: revoke user sessions: %w", err)
	}
	return nil
}

// HasAnyMember reports whether the workspace has at least one membership. A
// workspace with none is unclaimed, and registration bootstraps its owner.
func (r *PostgresRepository) HasAnyMember(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM memberships WHERE tenant_id = $1)`, tenantID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("auth: check memberships: %w", err)
	}
	return exists, nil
}

/*
MembersByEmail maps addresses to the people in this workspace who hold them.

One query for the whole list — `= ANY($2)` and not an address at a time, because
the caller is importing a cohort and a query per learner is the N+1 this codebase
does not write.

Active memberships only. A suspended member is not somebody a course may be given
to, and an address nobody here holds simply has no key: the caller reports that as
"not a member", which is the commonest and most useful line in an import's report.
*/
func (r *PostgresRepository) MembersByEmail(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, emails []string) (map[string]uuid.UUID, error) {
	rows, err := tx.Query(ctx,
		`SELECT lower(u.email), u.id
		 FROM memberships m
		 JOIN users u ON u.id = m.user_id
		 WHERE m.tenant_id = $1 AND m.status = 'active' AND lower(u.email) = ANY($2)`,
		tenantID, emails)
	if err != nil {
		return nil, fmt.Errorf("auth: members by email: %w", err)
	}
	defer rows.Close()

	found := make(map[string]uuid.UUID, len(emails))
	for rows.Next() {
		var (
			email  string
			userID uuid.UUID
		)
		if err := rows.Scan(&email, &userID); err != nil {
			return nil, fmt.Errorf("auth: scan member by email: %w", err)
		}
		found[email] = userID
	}
	return found, rows.Err()
}
