// Package auth owns identity, sessions, and authorisation.
//
// It knows nothing about HTTP. It returns its own sentinel errors, which the
// transport layer maps to status codes.
package auth

import (
	"errors"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors.
//
// ErrInvalidCredentials covers a missing user, a wrong password, and a suspended
// membership alike. The caller must not distinguish them: an error that says
// "no such user" is an account-enumeration oracle, and on a school's tenant that
// is a roster.
var (
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	ErrEmailTaken         = errors.New("auth: email is already registered")
	ErrSessionInvalid     = errors.New("auth: session is invalid or expired")
	ErrSessionReused      = errors.New("auth: refresh token was already used")
	ErrForbidden          = errors.New("auth: not permitted")
	ErrUnauthenticated    = errors.New("auth: not authenticated")
)

// Audit actions emitted by this package. Declared here rather than in the audit
// package, because a domain package may not import a sibling — and because the
// vocabulary of events belongs to whoever emits them.
const (
	ActionUserRegistered       = "user.registered"
	ActionUserLoggedIn         = "user.logged_in"
	ActionUserLoginFailed      = "user.login_failed"
	ActionUserLoggedOut        = "user.logged_out"
	ActionSessionRefreshed     = "session.refreshed"
	ActionSessionReuseDetected = "session.reuse_detected"
)

// Roles, ordered from most to least privileged.
const (
	RoleOwner      = "owner"
	RoleAdmin      = "admin"
	RoleInstructor = "instructor"
	RoleStudent    = "student"
)

// Permissions. A permission names a capability, never a role, so that changing
// who may do a thing does not mean changing the code that does it.
const (
	PermCourseRead    = "course:read"
	PermCourseWrite   = "course:write"
	PermCoursePublish = "course:publish"

	// PermSubmissionGrade marks an essay or an assignment by hand. Deliberately
	// separate from course:write: a teaching assistant marks work without being
	// able to rewrite the course, and an author is not thereby a marker.
	PermSubmissionGrade = "submission:grade"
	PermUserRead        = "user:read"
	PermUserManage      = "user:manage"
	PermTenantManage    = "tenant:manage"

	// PermForumModerate creates community spaces and pins, locks, or removes any
	// thread or post. Separate from course:write: moderating the community is not
	// the same job as authoring a course, though the same roles happen to do both.
	PermForumModerate = "forum:moderate"

	// PermAcademicsManage sets up the institution's structure — its type, academic
	// years and terms, classes and sections. An administrative act, not a teaching
	// one: an instructor authors courses but does not redraw the school.
	PermAcademicsManage = "academics:manage"
)

// rolePermissions is the entire authorisation model. It is a map rather than a
// hierarchy on purpose: "admin can do everything an instructor can" is true today
// and is exactly the assumption that quietly breaks when a role is added.
var rolePermissions = map[string]map[string]bool{
	RoleOwner: {
		PermCourseRead: true, PermCourseWrite: true, PermCoursePublish: true,
		PermSubmissionGrade: true,
		PermUserRead:        true, PermUserManage: true, PermTenantManage: true,
		PermForumModerate: true, PermAcademicsManage: true,
	},
	RoleAdmin: {
		PermCourseRead: true, PermCourseWrite: true, PermCoursePublish: true,
		PermSubmissionGrade: true,
		PermUserRead:        true, PermUserManage: true,
		PermForumModerate: true, PermAcademicsManage: true,
	},
	RoleInstructor: {
		PermCourseRead: true, PermCourseWrite: true, PermCoursePublish: true,
		PermSubmissionGrade: true,
		PermUserRead:        true,
		PermForumModerate:   true,
	},
	RoleStudent: {
		PermCourseRead: true,
	},
}

// Can reports whether role grants permission. An unknown role grants nothing.
func Can(role, permission string) bool {
	return rolePermissions[role][permission]
}

// User is a person. Users are global: one account, however many tenants.
type User struct {
	ID              uuid.UUID
	Email           string
	Name            string
	EmailVerifiedAt *time.Time
	CreatedAt       time.Time
}

// Membership binds a user to a tenant and carries their role there.
type Membership struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	UserID   uuid.UUID
	Role     string
	Status   string
}

// Active reports whether the membership may be used to authenticate.
func (m Membership) Active() bool { return m.Status == "active" }

// Principal is the authenticated caller: who they are, where, and as what.
//
// It is derived from a verified access token on every request. It is never read
// from the database on the hot path, which is the entire reason a stateless
// access token is worth its complexity.
type Principal struct {
	UserID    uuid.UUID
	TenantID  uuid.UUID
	SessionID uuid.UUID
	Role      string
}

// Can reports whether the principal holds permission.
func (p Principal) Can(permission string) bool { return Can(p.Role, permission) }

// TokenPair is what a successful login or refresh returns.
type TokenPair struct {
	// AccessToken is a short-lived JWT. It is a bearer credential: anyone holding
	// it is the user, until it expires.
	AccessToken string
	ExpiresIn   int

	// RefreshToken is opaque and long-lived. Only its SHA-256 digest is stored, so
	// a database dump does not hand over live sessions.
	RefreshToken string
}

// Credentials identify a login attempt.
type Credentials struct {
	Email    string
	Password string
}

// RequestContext is the ambient detail every auditable action records.
type RequestContext struct {
	IP        netip.Addr
	UserAgent string
}
