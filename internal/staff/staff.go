// Package staff models the people who run an institution — teachers and the office
// around them. It knows nothing about HTTP, and links to a user account by id when
// the person logs in, or stands alone when they do not.
package staff

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Sentinels. internal/httpapi maps each to a status; add a line to errors_test.go
// in the same commit as a new one.
var (
	ErrNotFound     = errors.New("staff: not found")
	ErrStaffNoTaken = errors.New("staff: that staff number is already used")
	ErrInvalidStaff = errors.New("staff: the staff record is not valid")
	ErrInvalidPage  = errors.New("staff: the page cursor is not valid")
)

// Roles a staff member fills. This is what they do, not what they may do — login
// permission lives on the membership.
const (
	RoleTeacher    = "teacher"
	RolePrincipal  = "principal"
	RoleAdmin      = "admin"
	RoleAccountant = "accountant"
	RoleLibrarian  = "librarian"
	RoleSupport    = "support"
)

// ValidRole reports whether r is a role a staff record may hold.
func ValidRole(r string) bool {
	switch r {
	case RoleTeacher, RolePrincipal, RoleAdmin, RoleAccountant, RoleLibrarian, RoleSupport:
		return true
	default:
		return false
	}
}

// MaxRoster bounds the staff list.
const MaxRoster = 100

// Audit action.
const ActionHired = "staff.hired"

// Staff is one person on the payroll.
type Staff struct {
	ID        uuid.UUID
	StaffNo   string
	FullName  string
	Role      string
	Email     string
	Phone     string
	UserID    *uuid.UUID
	Status    string
	JoinedOn  *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewStaff is a record to create.
type NewStaff struct {
	StaffNo  string
	FullName string
	Role     string
	Email    string
	Phone    string
	JoinedOn *time.Time
}

// StaffPatch edits a record; a nil field is left unchanged.
type StaffPatch struct {
	FullName *string
	Role     *string
	Email    *string
	Phone    *string
	Status   *string
	JoinedOn *time.Time
}

// RosterFilter narrows the staff list.
type RosterFilter struct {
	Role string
}

func (n *NewStaff) validate() error {
	if n.FullName == "" {
		return fmt.Errorf("%w: name them", ErrInvalidStaff)
	}
	if n.Role == "" {
		n.Role = RoleTeacher
	}
	if !ValidRole(n.Role) {
		return fmt.Errorf("%w: %q is not a role", ErrInvalidStaff, n.Role)
	}
	return nil
}

func (p StaffPatch) validate() error {
	if p.Role != nil && !ValidRole(*p.Role) {
		return fmt.Errorf("%w: %q is not a role", ErrInvalidStaff, *p.Role)
	}
	if p.Status != nil && *p.Status != "active" && *p.Status != "inactive" {
		return fmt.Errorf("%w: status is active or inactive", ErrInvalidStaff)
	}
	return nil
}

// cursor is the keyset position in the roster: staff are listed by name, so a page
// resumes at the row just after this name and id.
type cursor struct {
	Name string    `json:"n"`
	ID   uuid.UUID `json:"i"`
}

// PageParams is a request for one page of the roster.
type PageParams struct {
	Limit  int
	Cursor string
}

const (
	defaultPageLimit = 50
	maxPageLimit     = 100
)

func (p PageParams) clamp() int {
	switch {
	case p.Limit <= 0:
		return defaultPageLimit
	case p.Limit > maxPageLimit:
		return maxPageLimit
	default:
		return p.Limit
	}
}

func (p PageParams) decode() (*cursor, error) {
	if p.Cursor == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(p.Cursor)
	if err != nil {
		return nil, ErrInvalidPage
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, ErrInvalidPage
	}
	return &c, nil
}

func encodeCursor(s Staff) string {
	raw, _ := json.Marshal(cursor{Name: s.FullName, ID: s.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// Page is one page of the roster with a keyset cursor to the next.
type Page struct {
	Staff      []Staff
	NextCursor string
	HasMore    bool
}
