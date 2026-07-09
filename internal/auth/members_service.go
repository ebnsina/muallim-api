package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MembershipRepository is the persistence contract for invitations and members.
// Separated from Repository only for readability; PostgresRepository satisfies
// both.
type MembershipRepository interface {
	CreateInvitation(ctx context.Context, tx pgx.Tx, inv Invitation, digest []byte) error
	InvitationByDigest(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, digest []byte) (Invitation, error)
	MarkInvitationAccepted(ctx context.Context, tx pgx.Tx, tenantID, invitationID uuid.UUID) (bool, error)
	RevokeInvitation(ctx context.Context, tx pgx.Tx, tenantID, invitationID uuid.UUID) (bool, error)
	ListInvitations(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit int) ([]Invitation, error)

	InvitedUserByEmail(ctx context.Context, tx pgx.Tx, email string) (User, string, bool, error)
	MembershipFor(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) (Membership, error)
	ListMembers(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit int) ([]Member, error)

	CountOwners(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (int, error)
	LockMemberships(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error
	UpdateMemberRole(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, role string) error
	RemoveMember(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) error
	RevokeAllUserSessions(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) error
	HasAnyMember(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (bool, error)
}

// Invite creates an invitation and returns the raw token exactly once.
//
// The token is never stored and never retrievable again — only its digest is
// kept. A lost invitation is re-sent by creating a new one, which is the same
// property that makes a stolen database useless for joining workspaces.
//
// Sending the email is not this function's job. It returns the token; a caller
// hands it to whatever delivers it. Until a mailer exists, that caller is a human
// with a link.
func (s *Service) Invite(ctx context.Context, p Principal, email, role string, rc RequestContext) (Invitation, string, error) {
	email = normaliseEmail(email)
	if email == "" {
		return Invitation{}, "", fmt.Errorf("%w: email is required", ErrInvitationInvalid)
	}
	if !ValidRole(role) {
		return Invitation{}, "", fmt.Errorf("%w: unknown role %q", ErrInvitationInvalid, role)
	}

	// Only an owner may mint another owner. Otherwise an admin promotes themselves
	// by inviting an alias.
	if role == RoleOwner && p.Role != RoleOwner {
		return Invitation{}, "", fmt.Errorf("%w: only an owner may invite an owner", ErrForbidden)
	}

	token, digest, err := newRefreshToken() // same generator: 256 bits, opaque, digest-stored
	if err != nil {
		return Invitation{}, "", err
	}

	inv := Invitation{
		ID:        uuid.New(),
		TenantID:  p.TenantID,
		Email:     email,
		Role:      role,
		InvitedBy: &p.UserID,
		ExpiresAt: s.now().Add(InvitationTTL),
	}

	err = s.db.WithTenant(ctx, p.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Whether the address already belongs to a member cannot be checked here:
		// the users SELECT policy hides an account from a workspace it has no
		// membership in, and the invitation that would reveal it does not exist yet.
		// AcceptInvitation catches it instead, where the account is visible.
		if err := s.members.CreateInvitation(ctx, tx, inv, digest); err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, p.TenantID, AuditEntry{
			ActorID: &p.UserID, Action: ActionInvitationCreated,
			TargetType: "invitation", TargetID: inv.ID.String(),
			IP: rc.IP, UserAgent: rc.UserAgent,
			Metadata: map[string]any{"email": email, "role": role},
		})
	})
	if err != nil {
		return Invitation{}, "", err
	}
	return inv, token, nil
}

// AcceptInvitation exchanges an invitation token for membership and a session.
//
// Two paths, and the difference is where the security lives:
//
//   - The address has no account. The caller supplies a password and a name, and
//     an account is created.
//   - The address already has a global account, on some other workspace. The
//     caller must supply *that account's existing password*. Accepting an
//     invitation must not be a way to take over an account merely by knowing the
//     address it was sent to — the invitation proves the workspace wants them, not
//     that the requester is them.
func (s *Service) AcceptInvitation(ctx context.Context, tenantID uuid.UUID, token, pw, name string, rc RequestContext) (TokenPair, User, string, error) {
	if err := validateRefreshTokenFormat(token); err != nil {
		return TokenPair{}, User{}, "", ErrInvitationInvalid
	}
	digest := hashToken(token)

	var (
		pair TokenPair
		user User
		role string
	)

	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		inv, err := s.members.InvitationByDigest(ctx, tx, tenantID, digest)
		if err != nil {
			return err
		}
		if !inv.Pending(s.now()) {
			return ErrInvitationInvalid
		}

		existing, hash, found, err := s.members.InvitedUserByEmail(ctx, tx, inv.Email)
		if err != nil {
			return err
		}

		switch {
		case found:
			// Already a member? The invitation is meaningless and the unique index on
			// (tenant_id, user_id) would reject the membership anyway.
			if _, err := s.members.MembershipFor(ctx, tx, tenantID, existing.ID); err == nil {
				return ErrAlreadyMember
			} else if !errors.Is(err, ErrNotMember) {
				return err
			}

			// Prove ownership of the existing account. The invitation proves the
			// workspace wants this address; it does not prove the requester is it.
			ok, err := VerifyPassword(pw, hash)
			if err != nil {
				return fmt.Errorf("auth: verify password: %w", err)
			}
			if !ok {
				return ErrInvalidCredentials
			}
			user = existing

		default:
			if err := ValidatePassword(pw); err != nil {
				return err
			}
			newHash, err := HashPassword(pw)
			if err != nil {
				return err
			}
			user = User{ID: uuid.New(), Email: inv.Email, Name: name}
			if err := s.repo.CreateUser(ctx, tx, user, newHash); err != nil {
				return err
			}
		}

		// Close the invitation before creating the membership. The conditional
		// UPDATE returns zero rows if another request accepted it first, so a race
		// cannot produce two memberships from one invitation.
		accepted, err := s.members.MarkInvitationAccepted(ctx, tx, tenantID, inv.ID)
		if err != nil {
			return err
		}
		if !accepted {
			return ErrInvitationInvalid
		}

		if err := s.repo.CreateMembership(ctx, tx, Membership{
			ID: uuid.New(), TenantID: tenantID, UserID: user.ID, Role: inv.Role, Status: "active",
		}); err != nil {
			return err
		}
		role = inv.Role

		pair, err = s.startSession(ctx, tx, tenantID, user.ID, role, rc)
		if err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &user.ID, Action: ActionInvitationAccepted,
			TargetType: "invitation", TargetID: inv.ID.String(),
			IP: rc.IP, UserAgent: rc.UserAgent,
			Metadata: map[string]any{"role": role, "new_account": !found},
		})
	})
	if err != nil {
		return TokenPair{}, User{}, "", err
	}
	return pair, user, role, nil
}

// RevokeInvitationByID withdraws an outstanding invitation.
func (s *Service) RevokeInvitationByID(ctx context.Context, p Principal, invitationID uuid.UUID, rc RequestContext) error {
	return s.db.WithTenant(ctx, p.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		revoked, err := s.members.RevokeInvitation(ctx, tx, p.TenantID, invitationID)
		if err != nil {
			return err
		}
		if !revoked {
			return ErrInvitationInvalid
		}
		return s.audit.Record(ctx, tx, p.TenantID, AuditEntry{
			ActorID: &p.UserID, Action: ActionInvitationRevoked,
			TargetType: "invitation", TargetID: invitationID.String(),
			IP: rc.IP, UserAgent: rc.UserAgent,
		})
	})
}

// Invitations lists the workspace's invitations, newest first.
func (s *Service) Invitations(ctx context.Context, p Principal, limit int) ([]Invitation, error) {
	var out []Invitation
	err := s.db.WithTenantReadOnly(ctx, p.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.members.ListInvitations(ctx, tx, p.TenantID, limit)
		return err
	})
	return out, err
}

// Members lists the workspace's members.
func (s *Service) Members(ctx context.Context, p Principal, limit int) ([]Member, error) {
	var out []Member
	err := s.db.WithTenantReadOnly(ctx, p.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.members.ListMembers(ctx, tx, p.TenantID, limit)
		return err
	})
	return out, err
}

// ChangeMemberRole promotes or demotes a member.
//
// Two invariants. Nobody edits their own role — an admin would promote themselves
// to owner. And a workspace never loses its last owner, or nobody can administer
// it again.
func (s *Service) ChangeMemberRole(ctx context.Context, p Principal, userID uuid.UUID, role string, rc RequestContext) error {
	if !ValidRole(role) {
		return fmt.Errorf("%w: unknown role %q", ErrForbidden, role)
	}
	if userID == p.UserID {
		return ErrSelfModification
	}
	if role == RoleOwner && p.Role != RoleOwner {
		return fmt.Errorf("%w: only an owner may promote to owner", ErrForbidden)
	}

	return s.db.WithTenant(ctx, p.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Lock first: without it two concurrent demotions each see two owners, each
		// proceed, and the workspace ends with none.
		if err := s.members.LockMemberships(ctx, tx, p.TenantID); err != nil {
			return err
		}

		current, err := s.members.MembershipFor(ctx, tx, p.TenantID, userID)
		if err != nil {
			return err
		}
		if current.Role == role {
			return nil // idempotent
		}

		if current.Role == RoleOwner {
			owners, err := s.members.CountOwners(ctx, tx, p.TenantID)
			if err != nil {
				return err
			}
			if owners <= 1 {
				return ErrLastOwner
			}
		}

		if err := s.members.UpdateMemberRole(ctx, tx, p.TenantID, userID, role); err != nil {
			return err
		}

		// Their access token still carries the old role until it expires. Revoking
		// their sessions makes a demotion take effect now rather than in fifteen
		// minutes, which is the whole point of demoting somebody.
		if err := s.members.RevokeAllUserSessions(ctx, tx, p.TenantID, userID); err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, p.TenantID, AuditEntry{
			ActorID: &p.UserID, Action: ActionMemberRoleChanged,
			TargetType: "user", TargetID: userID.String(),
			IP: rc.IP, UserAgent: rc.UserAgent,
			Metadata: map[string]any{"from": current.Role, "to": role},
		})
	})
}

// RemoveMember removes a user from the workspace.
//
// Their global account survives: other workspaces may hold memberships against
// it. Removing somebody from a workspace is not erasing them from the platform.
func (s *Service) RemoveMember(ctx context.Context, p Principal, userID uuid.UUID, rc RequestContext) error {
	if userID == p.UserID {
		return ErrSelfModification
	}

	return s.db.WithTenant(ctx, p.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.members.LockMemberships(ctx, tx, p.TenantID); err != nil {
			return err
		}

		current, err := s.members.MembershipFor(ctx, tx, p.TenantID, userID)
		if err != nil {
			return err
		}

		if current.Role == RoleOwner {
			owners, err := s.members.CountOwners(ctx, tx, p.TenantID)
			if err != nil {
				return err
			}
			if owners <= 1 {
				return ErrLastOwner
			}
		}

		if err := s.members.RevokeAllUserSessions(ctx, tx, p.TenantID, userID); err != nil {
			return err
		}
		if err := s.members.RemoveMember(ctx, tx, p.TenantID, userID); err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, p.TenantID, AuditEntry{
			ActorID: &p.UserID, Action: ActionMemberRemoved,
			TargetType: "user", TargetID: userID.String(),
			IP: rc.IP, UserAgent: rc.UserAgent,
			Metadata: map[string]any{"role": current.Role},
		})
	})
}
