package auth

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Invitation errors.
var (
	ErrInvitationInvalid  = errors.New("auth: invitation is invalid, expired, or already used")
	ErrAlreadyMember      = errors.New("auth: already a member of this workspace")
	ErrInvitationPending  = errors.New("auth: an invitation for that address is already outstanding")
	ErrRegistrationClosed = errors.New("auth: this workspace is invitation-only")
	ErrLastOwner          = errors.New("auth: a workspace must keep at least one owner")
	ErrNotMember          = errors.New("auth: not a member of this workspace")
	ErrSelfModification   = errors.New("auth: you cannot change your own role or remove yourself")
)

// Audit actions for membership lifecycle.
const (
	ActionInvitationCreated  = "invitation.created"
	ActionInvitationAccepted = "invitation.accepted"
	ActionInvitationRevoked  = "invitation.revoked"
	ActionMemberRoleChanged  = "member.role_changed"
	ActionMemberRemoved      = "member.removed"
)

// InvitationTTL bounds how long a link is a valid credential. Long enough for
// somebody to find the email on Monday; short enough that a link forwarded into
// a group chat two months ago is dead.
const InvitationTTL = 7 * 24 * time.Hour

// Invitation is an offer of membership in a workspace.
type Invitation struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	Email    string
	Role     string

	InvitedBy *uuid.UUID
	ExpiresAt time.Time

	AcceptedAt *time.Time
	RevokedAt  *time.Time
	CreatedAt  time.Time
}

// Pending reports whether the invitation may still be accepted.
func (i Invitation) Pending(now time.Time) bool {
	return i.AcceptedAt == nil && i.RevokedAt == nil && now.Before(i.ExpiresAt)
}

// Status renders the invitation's state for a client.
func (i Invitation) Status(now time.Time) string {
	switch {
	case i.AcceptedAt != nil:
		return "accepted"
	case i.RevokedAt != nil:
		return "revoked"
	case !now.Before(i.ExpiresAt):
		return "expired"
	default:
		return "pending"
	}
}

// Member is a user together with their role in a workspace.
type Member struct {
	User User
	Role string
	// Status of the membership: active or suspended.
	Status string
}

// ValidRole reports whether role is one this system knows.
func ValidRole(role string) bool {
	_, ok := rolePermissions[role]
	return ok
}
