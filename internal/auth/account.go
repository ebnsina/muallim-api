package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Audit actions for a person changing their own account.
const (
	ActionNameChanged     = "user.renamed"
	ActionPasswordChanged = "password.changed"
)

// MaxNameLength is what a display name may be. Long enough for a real name in any
// script; short enough that it cannot be used as a message board.
const MaxNameLength = 120

// ErrNameInvalid means a name that is empty once trimmed, or longer than the column.
var ErrNameInvalid = errors.New("auth: the name is empty or too long")

/*
Rename changes what a person is called.

Their own row, their own name, and no permission to check: this is the one thing
in the system that needs no authority beyond being signed in.
*/
func (s *Service) Rename(ctx context.Context, p Principal, name string, rc RequestContext) (User, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > MaxNameLength {
		return User{}, ErrNameInvalid
	}

	var user User
	err := s.db.WithTenant(ctx, p.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.repo.SetName(ctx, tx, p.TenantID, p.UserID, name); err != nil {
			return err
		}

		var err error
		user, _, err = s.repo.UserByID(ctx, tx, p.TenantID, p.UserID)
		if err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, p.TenantID, AuditEntry{
			ActorID: &p.UserID, Action: ActionNameChanged,
			TargetType: "user", TargetID: p.UserID.String(),
			IP: rc.IP, UserAgent: rc.UserAgent,
		})
	})
	if err != nil {
		return User{}, err
	}
	return user, nil
}

/*
ChangePassword replaces a password for somebody who can already prove they know it.

The current one is checked first, and that check is the whole point: a stolen access
token would otherwise be enough to lock the owner out of their own account for good.
It is the same refusal as a wrong password at the door — ErrInvalidCredentials — and
it says nothing more than that.

Every other session then ends. The person who just changed their password is,
overwhelmingly often, a person who thinks somebody else is in their account, and the
answer to that has to be "not any more" rather than "not on new logins". The browser
in front of us keeps its session: signing somebody out of the tab where they were
being careful is a punishment for being careful.

Both hashes are computed outside the transaction. Argon2id holds 64 MiB while it
runs, and a transaction held open across it holds a pooled connection with it.
*/
func (s *Service) ChangePassword(ctx context.Context, p Principal, current, next string, rc RequestContext) error {
	if err := ValidatePassword(next); err != nil {
		return err
	}

	hash, err := HashPassword(next)
	if err != nil {
		return err
	}

	// The rejection is carried out in a variable, not returned from the transaction.
	// Returning it would roll back the audit entry recording the rejection — the one
	// line somebody investigating an intrusion actually needs.
	var refused error

	err = s.db.WithTenant(ctx, p.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		stored, err := s.repo.PasswordHash(ctx, tx, p.TenantID, p.UserID)
		if err != nil {
			return err
		}

		ok, err := VerifyPassword(current, stored)
		if err != nil {
			return fmt.Errorf("auth: verify password: %w", err)
		}
		if !ok {
			refused = ErrInvalidCredentials
			return s.audit.Record(ctx, tx, p.TenantID, AuditEntry{
				ActorID: &p.UserID, Action: ActionPasswordChanged,
				TargetType: "user", TargetID: p.UserID.String(),
				IP: rc.IP, UserAgent: rc.UserAgent,
				Metadata: map[string]any{"outcome": "refused"},
			})
		}

		if err := s.creds.SetPasswordHash(ctx, tx, p.TenantID, p.UserID, hash); err != nil {
			return err
		}

		// Every other session in this workspace, now — except the one asking.
		if err := s.members.RevokeOtherUserSessions(ctx, tx, p.TenantID, p.UserID, p.SessionID); err != nil {
			return err
		}

		// And every session in every other workspace, shortly. Same transaction, so the
		// job exists if and only if the password changed.
		if err := s.jobs.RevokeSessionsEverywhere(ctx, tx, p.UserID); err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, p.TenantID, AuditEntry{
			ActorID: &p.UserID, Action: ActionPasswordChanged,
			TargetType: "user", TargetID: p.UserID.String(),
			IP: rc.IP, UserAgent: rc.UserAgent,
		})
	})
	if err != nil {
		return err
	}
	return refused
}
