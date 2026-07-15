package academics

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Student sentinels.
var (
	// ErrInvalidStudent means a student's admission number or name was refused.
	ErrInvalidStudent = errors.New("academics: the student is not valid")

	// ErrAdmissionTaken means the admission number is already on another student.
	ErrAdmissionTaken = errors.New("academics: that admission number is already used")

	// ErrInvalidGuardian means a guardian's name was missing.
	ErrInvalidGuardian = errors.New("academics: the guardian is not valid")

	// ErrInvalidPage means a roster cursor this API did not issue.
	ErrInvalidPage = errors.New("academics: the page cursor is not valid")
)

// Student statuses. A student on a roll is active until they leave.
const (
	StatusActive      = "active"
	StatusInactive    = "inactive"
	StatusGraduated   = "graduated"
	StatusTransferred = "transferred"
)

// MaxRosterPage bounds a page of the student roster.
const MaxRosterPage = 100

// ValidStatus reports whether s is a status a student may hold.
func ValidStatus(s string) bool {
	switch s {
	case StatusActive, StatusInactive, StatusGraduated, StatusTransferred:
		return true
	default:
		return false
	}
}

// Student is one person on a roll.
type Student struct {
	ID           uuid.UUID
	AdmissionNo  string
	FullName     string
	GradeLevelID *uuid.UUID
	SectionID    *uuid.UUID
	Roll         int
	Status       string
	UserID       *uuid.UUID
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// NewStudent describes a student to admit.
type NewStudent struct {
	AdmissionNo  string
	FullName     string
	GradeLevelID *uuid.UUID
	SectionID    *uuid.UUID
	Roll         int
}

// StudentPatch edits a student. A nil field is left alone.
type StudentPatch struct {
	FullName     *string
	GradeLevelID *uuid.UUID
	SectionID    *uuid.UUID
	Roll         *int
	Status       *string
}

// Guardian is a student's contact — a parent or a carer.
type Guardian struct {
	ID        uuid.UUID
	FullName  string
	Phone     string
	Email     string
	Relation  string
	IsPrimary bool
	CreatedAt time.Time
}

// NewGuardian describes a guardian to add to a student.
type NewGuardian struct {
	FullName  string
	Phone     string
	Email     string
	Relation  string
	IsPrimary bool
}

// RosterFilter narrows the student roster.
type RosterFilter struct {
	GradeLevelID *uuid.UUID
}

// RosterPage is one page of the roster, and where the next begins.
type RosterPage struct {
	Students   []Student
	NextCursor string
	HasMore    bool
}

// PageParams is how many, and from where.
type PageParams struct {
	Limit  int
	Cursor string
}

// cursor is a keyset position: the (full_name, id) of the last row of the page
// before. JSON-encoded so a name carrying any separator is safe.
type cursor struct {
	Name string    `json:"n"`
	ID   uuid.UUID `json:"i"`
}

func (c cursor) encode() string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(token string) (cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: not base64", ErrInvalidPage)
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return cursor{}, fmt.Errorf("%w: malformed", ErrInvalidPage)
	}
	return c, nil
}

func (p PageParams) clamp() (int, *cursor, error) {
	limit := p.Limit
	if limit <= 0 || limit > MaxRosterPage {
		limit = MaxRosterPage
	}
	if p.Cursor == "" {
		return limit, nil, nil
	}
	after, err := decodeCursor(p.Cursor)
	if err != nil {
		return 0, nil, err
	}
	return limit, &after, nil
}

// paginate turns limit+1 rows into a page — the extra row answers "is there more"
// without a COUNT(*).
func paginate(rows []Student, limit int) RosterPage {
	if len(rows) <= limit {
		return RosterPage{Students: rows}
	}
	rows = rows[:limit]
	last := rows[len(rows)-1]
	return RosterPage{
		Students:   rows,
		HasMore:    true,
		NextCursor: cursor{Name: last.FullName, ID: last.ID}.encode(),
	}
}

func (n NewStudent) validate() error {
	if strings.TrimSpace(n.AdmissionNo) == "" {
		return fmt.Errorf("%w: a student needs an admission number", ErrInvalidStudent)
	}
	if strings.TrimSpace(n.FullName) == "" {
		return fmt.Errorf("%w: a student needs a name", ErrInvalidStudent)
	}
	if n.Roll < 0 {
		return fmt.Errorf("%w: a roll cannot be negative", ErrInvalidStudent)
	}
	return nil
}

func (p StudentPatch) validate() error {
	if p.FullName != nil && strings.TrimSpace(*p.FullName) == "" {
		return fmt.Errorf("%w: a student needs a name", ErrInvalidStudent)
	}
	if p.Roll != nil && *p.Roll < 0 {
		return fmt.Errorf("%w: a roll cannot be negative", ErrInvalidStudent)
	}
	if p.Status != nil && !ValidStatus(*p.Status) {
		return fmt.Errorf("%w: %q is not a status", ErrInvalidStudent, *p.Status)
	}
	return nil
}

func (n NewGuardian) validate() error {
	if strings.TrimSpace(n.FullName) == "" {
		return fmt.Errorf("%w: a guardian needs a name", ErrInvalidGuardian)
	}
	return nil
}
