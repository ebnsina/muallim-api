package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrTokenInvalid covers a token that never existed, one that expired, and one
// already spent. The bearer of a link cannot tell which, and has no use for the
// distinction.
var ErrTokenInvalid = errors.New("auth: token is invalid, expired, or already used")

// Audit actions for the credential lifecycle.
const (
	ActionEmailVerificationSent  = "email.verification_sent"
	ActionEmailVerified          = "email.verified"
	ActionPasswordResetRequested = "password.reset_requested"
	ActionPasswordReset          = "password.reset"
)

// Token lifetimes.
//
// A reset token is the password. It is mailed in plaintext to an inbox we do not
// control, so it lives for an hour: long enough to read the email, short enough
// that a mailbox compromised next week is not also an account compromised next
// week. Verification only confirms an address, so it is cheap to make it a day.
const (
	EmailVerificationTTL = 24 * time.Hour
	PasswordResetTTL     = time.Hour
)

// Token purposes. These are stored, and constrained by a CHECK in the schema.
const (
	purposeEmailVerification = "email_verification"
	purposePasswordReset     = "password_reset"
)

// CredentialToken is a single-use proof of control over an email address.
type CredentialToken struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	UserID   uuid.UUID
	Purpose  string

	ExpiresAt  time.Time
	ConsumedAt *time.Time
	CreatedAt  time.Time
}

// Live reports whether the token may still be spent.
func (t CredentialToken) Live(now time.Time) bool {
	return t.ConsumedAt == nil && now.Before(t.ExpiresAt)
}

// CredentialRepository persists credential tokens and the user columns they
// authorise a change to. Declared here by its consumer.
type CredentialRepository interface {
	CreateToken(ctx context.Context, tx pgx.Tx, t CredentialToken, digest []byte) error
	SupersedeTokens(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, purpose string) error
	TokenByDigest(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, purpose string, digest []byte) (CredentialToken, error)
	ConsumeToken(ctx context.Context, tx pgx.Tx, tenantID, tokenID uuid.UUID) (bool, error)

	SetPasswordHash(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, hash string) error
	MarkEmailVerified(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, at time.Time) error
}

// Mailer enqueues the messages this package needs sent.
//
// Declared here, by its consumer, and satisfied by the comms package. auth must
// not import comms: sibling domain packages never depend on one another.
//
// Every method takes the caller's transaction, so the message is queued in the
// same transaction as the token it carries. A token that exists but was never
// mailed is a support ticket; a message mailed for a transaction that rolled
// back is a link to nothing.
type Mailer interface {
	SendVerification(ctx context.Context, tx pgx.Tx, to, name, token string, expiresIn time.Duration) error
	SendPasswordReset(ctx context.Context, tx pgx.Tx, to, name, token string, expiresIn time.Duration) error
	SendInvitation(ctx context.Context, tx pgx.Tx, to, workspace, token string, expiresIn time.Duration) error
}

// RequestPasswordReset mails a single-use reset link, if the address belongs to
// an active member of this workspace.
//
// It reports success either way. An endpoint that answers "no such account" is
// an enumeration oracle available to anyone, without a password and without a
// rate limit worth the name — and on a school's workspace, the answer is a
// roster. The two paths do differ slightly in latency, because the found path
// writes a token; that is what the rate limiter on this route is for.
//
// A suspended member is treated as absent. Resetting a password must not be a way
// to discover that an account was suspended, and it must not reactivate them.
func (s *Service) RequestPasswordReset(ctx context.Context, tenantID uuid.UUID, email string, rc RequestContext) error {
	email = normaliseEmail(email)
	if email == "" {
		return nil
	}

	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		user, membership, _, err := s.repo.CredentialsByEmail(ctx, tx, tenantID, email)
		switch {
		case errors.Is(err, ErrInvalidCredentials):
			// Nothing to send. The attempt is still recorded: a burst of resets against
			// addresses that do not exist is what enumeration looks like from here.
			return s.audit.Record(ctx, tx, tenantID, AuditEntry{
				Action: ActionPasswordResetRequested, TargetType: "email", TargetID: email,
				IP: rc.IP, UserAgent: rc.UserAgent,
				Metadata: map[string]any{"delivered": false},
			})
		case err != nil:
			return err
		}

		if !membership.Active() {
			return s.audit.Record(ctx, tx, tenantID, AuditEntry{
				Action: ActionPasswordResetRequested, TargetType: "email", TargetID: email,
				IP: rc.IP, UserAgent: rc.UserAgent,
				Metadata: map[string]any{"delivered": false},
			})
		}

		token, digest, err := newRefreshToken()
		if err != nil {
			return err
		}

		// The newest link is the only live one. Without this, every reset a user ever
		// requested stays valid until it expires.
		if err := s.creds.SupersedeTokens(ctx, tx, tenantID, user.ID, purposePasswordReset); err != nil {
			return err
		}

		if err := s.creds.CreateToken(ctx, tx, CredentialToken{
			ID: uuid.New(), TenantID: tenantID, UserID: user.ID,
			Purpose: purposePasswordReset, ExpiresAt: s.now().Add(PasswordResetTTL),
		}, digest); err != nil {
			return err
		}

		if err := s.mail.SendPasswordReset(ctx, tx, user.Email, user.Name, token, PasswordResetTTL); err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &user.ID, Action: ActionPasswordResetRequested,
			TargetType: "user", TargetID: user.ID.String(),
			IP: rc.IP, UserAgent: rc.UserAgent,
			Metadata: map[string]any{"delivered": true},
		})
	})
}

// ResetPassword spends a reset token and sets a new password.
//
// Every session in this workspace is revoked. Whoever forced the reset — the
// owner of the account or the person who stole it — the other one is now logged
// out and must present the new password.
//
// The revocation is bounded by the bound tenant, because sessions are. A person
// who is a member of two workspaces has their password changed globally and their
// other workspace's sessions left alive. That is a gap, and closing it needs a
// cross-tenant sweep through WithoutTenant.
func (s *Service) ResetPassword(ctx context.Context, tenantID uuid.UUID, token, password string, rc RequestContext) error {
	if err := validateRefreshTokenFormat(token); err != nil {
		return ErrTokenInvalid
	}
	if err := ValidatePassword(password); err != nil {
		return err
	}

	// Hashed before the transaction opens. Argon2id holds 64 MiB for the duration,
	// and a transaction held open across it holds a pooled connection with it.
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	digest := hashToken(token)

	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ct, err := s.creds.TokenByDigest(ctx, tx, tenantID, purposePasswordReset, digest)
		if err != nil {
			return err
		}
		if !ct.Live(s.now()) {
			return ErrTokenInvalid
		}

		// Spend the token before honouring it. The conditional UPDATE matches zero
		// rows if another request spent it first, so two clicks on one link cannot
		// both set a password.
		spent, err := s.creds.ConsumeToken(ctx, tx, tenantID, ct.ID)
		if err != nil {
			return err
		}
		if !spent {
			return ErrTokenInvalid
		}

		if err := s.creds.SetPasswordHash(ctx, tx, tenantID, ct.UserID, hash); err != nil {
			return err
		}
		if err := s.members.RevokeAllUserSessions(ctx, tx, tenantID, ct.UserID); err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &ct.UserID, Action: ActionPasswordReset,
			TargetType: "user", TargetID: ct.UserID.String(),
			IP: rc.IP, UserAgent: rc.UserAgent,
		})
	})
}

// VerifyEmail spends a verification token and marks the address confirmed.
func (s *Service) VerifyEmail(ctx context.Context, tenantID uuid.UUID, token string, rc RequestContext) error {
	if err := validateRefreshTokenFormat(token); err != nil {
		return ErrTokenInvalid
	}
	digest := hashToken(token)

	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ct, err := s.creds.TokenByDigest(ctx, tx, tenantID, purposeEmailVerification, digest)
		if err != nil {
			return err
		}
		if !ct.Live(s.now()) {
			return ErrTokenInvalid
		}

		spent, err := s.creds.ConsumeToken(ctx, tx, tenantID, ct.ID)
		if err != nil {
			return err
		}
		if !spent {
			return ErrTokenInvalid
		}

		if err := s.creds.MarkEmailVerified(ctx, tx, tenantID, ct.UserID, s.now()); err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &ct.UserID, Action: ActionEmailVerified,
			TargetType: "user", TargetID: ct.UserID.String(),
			IP: rc.IP, UserAgent: rc.UserAgent,
		})
	})
}

// ResendVerification mails a fresh verification link to the authenticated user.
//
// It is a no-op for an address already confirmed, rather than an error: a client
// that resends twice has done nothing wrong, and a second link would supersede
// the first anyway.
func (s *Service) ResendVerification(ctx context.Context, p Principal, rc RequestContext) error {
	return s.db.WithTenant(ctx, p.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		user, _, err := s.repo.UserByID(ctx, tx, p.TenantID, p.UserID)
		if err != nil {
			return err
		}
		if user.EmailVerifiedAt != nil {
			return nil
		}
		return s.issueVerification(ctx, tx, p.TenantID, user, rc)
	})
}

// issueVerification mints a verification token and queues its email, inside the
// caller's transaction.
func (s *Service) issueVerification(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, user User, rc RequestContext) error {
	token, digest, err := newRefreshToken()
	if err != nil {
		return err
	}

	if err := s.creds.SupersedeTokens(ctx, tx, tenantID, user.ID, purposeEmailVerification); err != nil {
		return err
	}

	if err := s.creds.CreateToken(ctx, tx, CredentialToken{
		ID: uuid.New(), TenantID: tenantID, UserID: user.ID,
		Purpose: purposeEmailVerification, ExpiresAt: s.now().Add(EmailVerificationTTL),
	}, digest); err != nil {
		return err
	}

	if err := s.mail.SendVerification(ctx, tx, user.Email, user.Name, token, EmailVerificationTTL); err != nil {
		return err
	}

	return s.audit.Record(ctx, tx, tenantID, AuditEntry{
		ActorID: &user.ID, Action: ActionEmailVerificationSent,
		TargetType: "user", TargetID: user.ID.String(),
		IP: rc.IP, UserAgent: rc.UserAgent,
	})
}
