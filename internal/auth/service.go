package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/platform/database"
)

// Repository is the persistence contract, declared here by its consumer.
type Repository interface {
	CreateUser(ctx context.Context, tx pgx.Tx, u User, passwordHash string) error
	CreateMembership(ctx context.Context, tx pgx.Tx, m Membership) error
	CredentialsByEmail(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, email string) (User, Membership, string, error)
	UserByID(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) (User, string, error)

	CreateSession(ctx context.Context, tx pgx.Tx, s Session, digest []byte, rc RequestContext) error
	SessionByDigest(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, digest []byte) (Session, error)
	RotateSession(ctx context.Context, tx pgx.Tx, tenantID, oldID, newID uuid.UUID) error
	RevokeFamily(ctx context.Context, tx pgx.Tx, tenantID, familyID uuid.UUID) (int64, error)
	RevokeSession(ctx context.Context, tx pgx.Tx, tenantID, sessionID uuid.UUID) error
}

// AuditRecorder appends to the audit trail inside the caller's transaction.
//
// Declared here, by its consumer, and satisfied by the audit package. auth must
// not import audit: sibling domain packages never depend on one another, and
// cmd/ does the wiring.
type AuditRecorder interface {
	Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e AuditEntry) error
}

// AuditEntry mirrors audit.Entry. Restating it here is the price of the
// dependency rule, and it is cheap: this package names what it needs, and the
// compiler checks that whatever is wired in satisfies it.
type AuditEntry struct {
	ActorID    *uuid.UUID
	Action     string
	TargetType string
	TargetID   string
	IP         netip.Addr
	UserAgent  string
	Metadata   map[string]any
}

// Service holds the authentication and authorisation rules.
type Service struct {
	db      *database.DB
	repo    Repository
	members MembershipRepository
	tokens  *TokenIssuer
	audit   AuditRecorder
	log     *slog.Logger

	now func() time.Time
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, members MembershipRepository, tokens *TokenIssuer, recorder AuditRecorder, log *slog.Logger) *Service {
	return &Service{db: db, repo: repo, members: members, tokens: tokens, audit: recorder, log: log, now: time.Now}
}

// Register claims an unclaimed workspace, creating its owner.
//
// It works only while the workspace has no members. After that, joining is by
// invitation, for two reasons. A person may already hold a global account from
// another workspace, and registration cannot link to it: the unique index on
// users.email rejects a second account, and the users SELECT policy hides the
// first. And an open registration endpoint that answers "that email is taken"
// tells any stranger whether an address exists somewhere on the platform.
func (s *Service) Register(ctx context.Context, tenantID uuid.UUID, c Credentials, name string, rc RequestContext) (TokenPair, User, string, error) {
	email := normaliseEmail(c.Email)
	if email == "" {
		return TokenPair{}, User{}, "", ErrInvalidCredentials
	}

	hash, err := HashPassword(c.Password)
	if err != nil {
		return TokenPair{}, User{}, "", err
	}

	var (
		pair    TokenPair
		user    User
		granted string
	)

	err = s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Serialise concurrent bootstrap attempts, so two simultaneous registrations
		// cannot both observe an empty workspace and both create an owner.
		if err := s.members.LockMemberships(ctx, tx, tenantID); err != nil {
			return err
		}

		claimed, err := s.members.HasAnyMember(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		if claimed {
			return ErrRegistrationClosed
		}

		user = User{ID: uuid.New(), Email: email, Name: name}

		if err := s.repo.CreateUser(ctx, tx, user, hash); err != nil {
			return err
		}

		role := RoleOwner // the first member of an unclaimed workspace owns it
		granted = role

		if err := s.repo.CreateMembership(ctx, tx, Membership{
			ID: uuid.New(), TenantID: tenantID, UserID: user.ID, Role: role, Status: "active",
		}); err != nil {
			return err
		}

		pair, err = s.startSession(ctx, tx, tenantID, user.ID, role, rc)
		if err != nil {
			return err
		}

		// The audit entry commits with the registration, or neither does.
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &user.ID, Action: ActionUserRegistered,
			TargetType: "user", TargetID: user.ID.String(),
			IP: rc.IP, UserAgent: rc.UserAgent,
			Metadata: map[string]any{"role": role},
		})
	})
	if err != nil {
		return TokenPair{}, User{}, "", err
	}
	return pair, user, granted, nil
}

// Login exchanges credentials for a token pair.
//
// A missing account, a wrong password, and a suspended membership all produce
// ErrInvalidCredentials, and all cost the same time: the unknown-account path
// hashes against a dummy digest. Without that, response latency answers "does
// this address have an account here?" for anyone who asks.
func (s *Service) Login(ctx context.Context, tenantID uuid.UUID, c Credentials, rc RequestContext) (TokenPair, User, string, error) {
	email := normaliseEmail(c.Email)

	var (
		pair    TokenPair
		user    User
		granted string

		// The rejection is carried out of the transaction rather than returned
		// from it. Returning an error would roll the transaction back — and the
		// failed-login audit entry with it, which is precisely the record we are
		// obliged to keep.
		rejected error
	)

	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		u, membership, hash, err := s.repo.CredentialsByEmail(ctx, tx, tenantID, email)
		if err != nil {
			if errors.Is(err, ErrInvalidCredentials) {
				BurnPasswordComparison(c.Password)
				rejected = ErrInvalidCredentials
				return s.recordFailedLogin(ctx, tx, tenantID, email, rc)
			}
			return err
		}

		ok, err := VerifyPassword(c.Password, hash)
		if err != nil {
			return fmt.Errorf("auth: verify password: %w", err)
		}
		if !ok || !membership.Active() {
			rejected = ErrInvalidCredentials
			return s.recordFailedLogin(ctx, tx, tenantID, email, rc)
		}

		pair, err = s.startSession(ctx, tx, tenantID, u.ID, membership.Role, rc)
		if err != nil {
			return err
		}
		user = u
		granted = membership.Role

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &u.ID, Action: ActionUserLoggedIn,
			TargetType: "user", TargetID: u.ID.String(),
			IP: rc.IP, UserAgent: rc.UserAgent,
		})
	})
	if err != nil {
		return TokenPair{}, User{}, "", err
	}
	if rejected != nil {
		return TokenPair{}, User{}, "", rejected
	}
	return pair, user, granted, nil
}

// recordFailedLogin audits the attempt. It returns only genuine failures, so the
// transaction commits and the audit entry survives; the caller signals the
// rejection to its own caller.
//
// The entry records the address that was tried, never the password. An audit log
// that captures failed passwords is a breach: users mistype the password for a
// different account into yours.
func (s *Service) recordFailedLogin(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, email string, rc RequestContext) error {
	return s.audit.Record(ctx, tx, tenantID, AuditEntry{
		Action: ActionUserLoginFailed, TargetType: "email", TargetID: email,
		IP: rc.IP, UserAgent: rc.UserAgent,
	})
}

// Refresh exchanges a refresh token for a new pair, rotating the old one away.
//
// Presenting a token that was already rotated is evidence of theft: the token
// only reaches an attacker by being stolen, and only reaches us twice if both the
// attacker and the victim used it. We cannot tell which party is which, so the
// entire session family is revoked and both must re-authenticate.
func (s *Service) Refresh(ctx context.Context, tenantID uuid.UUID, refreshToken string, rc RequestContext) (TokenPair, error) {
	if err := validateRefreshTokenFormat(refreshToken); err != nil {
		return TokenPair{}, ErrSessionInvalid
	}
	digest := hashToken(refreshToken)

	var (
		pair TokenPair

		// Carried out of the transaction rather than returned from it. Returning an
		// error rolls the transaction back — and with it the family revocation and
		// its audit entry, which would make reuse detection silently do nothing.
		rejected error
	)

	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		session, err := s.repo.SessionByDigest(ctx, tx, tenantID, digest)
		if err != nil {
			return err
		}

		// Rotated away, and presented anyway: the token reached us twice, so it
		// reached someone it should not have. Revoke the family.
		if session.Rotated() {
			rejected = ErrSessionReused
			return s.handleReuse(ctx, tx, tenantID, session, rc)
		}

		// Revoked without a successor — logged out, or swept when its family was
		// revoked. Invalid, but not evidence of anything.
		if !session.Live(s.now()) {
			return ErrSessionInvalid
		}

		_, role, err := s.repo.UserByID(ctx, tx, tenantID, session.UserID)
		if err != nil {
			// The membership was revoked while the session was live. The token is
			// valid; the access is not.
			return ErrSessionInvalid
		}

		newSessionID := uuid.New()
		token, newDigest, err := newRefreshToken()
		if err != nil {
			return err
		}

		// The successor is inserted before the predecessor points at it, because
		// sessions.replaced_by is a foreign key. Both statements share a
		// transaction, so a failure anywhere leaves the old token usable rather
		// than locking the user out.
		if err := s.repo.CreateSession(ctx, tx, Session{
			ID: newSessionID, TenantID: tenantID, UserID: session.UserID,
			FamilyID:  session.FamilyID, // the family survives rotation
			ExpiresAt: s.now().Add(RefreshTokenTTL),
		}, newDigest, rc); err != nil {
			return err
		}

		if err := s.repo.RotateSession(ctx, tx, tenantID, session.ID, newSessionID); err != nil {
			return err
		}

		access, expiresAt, err := s.tokens.Issue(Principal{
			UserID: session.UserID, TenantID: tenantID, SessionID: newSessionID, Role: role,
		})
		if err != nil {
			return err
		}

		pair = TokenPair{
			AccessToken:  access,
			ExpiresIn:    int(time.Until(expiresAt).Seconds()),
			RefreshToken: token,
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &session.UserID, Action: ActionSessionRefreshed,
			TargetType: "session", TargetID: newSessionID.String(),
			IP: rc.IP, UserAgent: rc.UserAgent,
		})
	})
	if err != nil {
		return TokenPair{}, err
	}
	if rejected != nil {
		return TokenPair{}, rejected
	}
	return pair, nil
}

// handleReuse revokes the whole family and audits the event.
func (s *Service) handleReuse(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, session Session, rc RequestContext) error {
	revoked, err := s.repo.RevokeFamily(ctx, tx, tenantID, session.FamilyID)
	if err != nil {
		return err
	}

	s.log.WarnContext(ctx, "refresh token reuse detected; revoking session family",
		slog.String("tenant_id", tenantID.String()),
		slog.String("user_id", session.UserID.String()),
		slog.String("family_id", session.FamilyID.String()),
		slog.Int64("sessions_revoked", revoked),
	)

	// Returns nil on success: the revocation and its audit entry must commit. The
	// caller reports ErrSessionReused after the transaction has landed.
	return s.audit.Record(ctx, tx, tenantID, AuditEntry{
		ActorID: &session.UserID, Action: ActionSessionReuseDetected,
		TargetType: "session_family", TargetID: session.FamilyID.String(),
		IP: rc.IP, UserAgent: rc.UserAgent,
		Metadata: map[string]any{"sessions_revoked": revoked},
	})
}

// Logout revokes one session. It does not touch the user's other devices.
func (s *Service) Logout(ctx context.Context, p Principal, rc RequestContext) error {
	return s.db.WithTenant(ctx, p.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.repo.RevokeSession(ctx, tx, p.TenantID, p.SessionID); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, p.TenantID, AuditEntry{
			ActorID: &p.UserID, Action: ActionUserLoggedOut,
			TargetType: "session", TargetID: p.SessionID.String(),
			IP: rc.IP, UserAgent: rc.UserAgent,
		})
	})
}

// Me returns the authenticated user's profile and current role.
//
// The role comes from the database, not from the token: a token minted before a
// demotion still carries the old role until it expires, and a profile page is
// exactly where that staleness would be visible.
func (s *Service) Me(ctx context.Context, p Principal) (User, string, error) {
	var (
		user User
		role string
	)
	err := s.db.WithTenantReadOnly(ctx, p.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		user, role, err = s.repo.UserByID(ctx, tx, p.TenantID, p.UserID)
		return err
	})
	if err != nil {
		return User{}, "", err
	}
	return user, role, nil
}

// Authorize returns ErrForbidden unless p holds permission.
//
// Authorisation lives in the service, not in middleware alone. Middleware
// establishes who you are; only the service knows what this operation requires.
func (s *Service) Authorize(p Principal, permission string) error {
	if !p.Can(permission) {
		return fmt.Errorf("%w: role %q lacks %q", ErrForbidden, p.Role, permission)
	}
	return nil
}

// Verify validates an access token. The transport layer calls it once per
// request; it touches no database.
func (s *Service) Verify(raw string) (Principal, error) { return s.tokens.Verify(raw) }

// startSession mints a fresh session family and its first token pair.
func (s *Service) startSession(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, role string, rc RequestContext) (TokenPair, error) {
	sessionID := uuid.New()

	token, digest, err := newRefreshToken()
	if err != nil {
		return TokenPair{}, err
	}

	if err := s.repo.CreateSession(ctx, tx, Session{
		ID: sessionID, TenantID: tenantID, UserID: userID,
		FamilyID:  sessionID, // a new login starts a new family, rooted at itself
		ExpiresAt: s.now().Add(RefreshTokenTTL),
	}, digest, rc); err != nil {
		return TokenPair{}, err
	}

	access, expiresAt, err := s.tokens.Issue(Principal{
		UserID: userID, TenantID: tenantID, SessionID: sessionID, Role: role,
	})
	if err != nil {
		return TokenPair{}, err
	}

	return TokenPair{
		AccessToken:  access,
		ExpiresIn:    int(time.Until(expiresAt).Seconds()),
		RefreshToken: token,
	}, nil
}

// normaliseEmail lowercases and trims, so "Ada@Example.com " and "ada@example.com"
// are one account rather than two.
func normaliseEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
