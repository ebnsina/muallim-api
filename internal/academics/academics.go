// Package academics is the institution layer: the calendar (years and terms) and
// the structure (classes and their sections) any school, college, madrasa, or
// coaching centre organises itself around. Students, attendance, exams and fees
// hang off these in later work.
package academics

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinels. Each gets a deliberate status in internal/httpapi; a new one needs a
// line in errors_test.go in the same commit.
var (
	ErrNotFound = errors.New("academics: not found")

	// ErrNameTaken means a year, class, or section already uses that name in this
	// workspace. Names are unique per scope, case-insensitively.
	ErrNameTaken = errors.New("academics: that name is already used")

	ErrInvalidYear    = errors.New("academics: the academic year is not valid")
	ErrInvalidTerm    = errors.New("academics: the term is not valid")
	ErrInvalidClass   = errors.New("academics: the class is not valid")
	ErrInvalidSection = errors.New("academics: the section is not valid")
	ErrInvalidSubject = errors.New("academics: the subject is not valid")

	// ErrInvalidInstitutionType means a type outside the known set was submitted.
	ErrInvalidInstitutionType = errors.New("academics: unknown institution type")
)

// Institution types. They set vocabulary and defaults, not schema.
const (
	TypeSchool   = "school"
	TypeCollege  = "college"
	TypeMadrasa  = "madrasa"
	TypeCoaching = "coaching"
)

// ValidInstitutionType reports whether t is a type this system knows.
func ValidInstitutionType(t string) bool {
	switch t {
	case TypeSchool, TypeCollege, TypeMadrasa, TypeCoaching:
		return true
	default:
		return false
	}
}

// Bounds. A workspace has few years and classes; these listings are bounded rather
// than paginated, and the cap is generous enough that reaching it is a mistake, not
// a school.
const (
	MaxYears    = 100
	MaxClasses  = 200
	MaxSubjects = 300
)

// Audit actions this package emits.
const (
	ActionYearCreated    = "academic_year.created"
	ActionYearSetCurrent = "academic_year.set_current"
	ActionClassCreated   = "grade_level.created"
	ActionAttendanceMark = "attendance.marked"
)

// AcademicYear is the calendar everything is scheduled within.
type AcademicYear struct {
	ID        uuid.UUID
	Name      string
	StartsOn  time.Time
	EndsOn    time.Time
	IsCurrent bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewAcademicYear describes a year to create.
type NewAcademicYear struct {
	Name     string
	StartsOn time.Time
	EndsOn   time.Time
}

// Term is a semester or quarter within a year, in the author's order.
type Term struct {
	ID             uuid.UUID
	AcademicYearID uuid.UUID
	Name           string
	StartsOn       time.Time
	EndsOn         time.Time
	Position       int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// NewTerm describes a term to append to a year.
type NewTerm struct {
	Name     string
	StartsOn time.Time
	EndsOn   time.Time
}

// GradeLevel is a class (Class 6, Dakhil 1st Year, a batch). Rank orders it by
// seniority.
type GradeLevel struct {
	ID        uuid.UUID
	Name      string
	Rank      int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewGradeLevel describes a class to create.
type NewGradeLevel struct {
	Name string
	Rank int
}

// Section is a division within a class (6-A). Capacity is a soft cap; zero is unset.
type Section struct {
	ID           uuid.UUID
	GradeLevelID uuid.UUID
	Name         string
	Capacity     int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// NewSection describes a section to add to a class.
type NewSection struct {
	Name     string
	Capacity int
}

// Subject is something the institution teaches. Exams mark against it later.
type Subject struct {
	ID        uuid.UUID
	Name      string
	Code      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewSubject describes a subject to add to the catalog.
type NewSubject struct {
	Name string
	Code string
}

func (n NewAcademicYear) validate() error {
	if strings.TrimSpace(n.Name) == "" {
		return fmt.Errorf("%w: a year needs a name", ErrInvalidYear)
	}
	if !n.EndsOn.After(n.StartsOn) {
		return fmt.Errorf("%w: the year must end after it starts", ErrInvalidYear)
	}
	return nil
}

func (n NewTerm) validate() error {
	if strings.TrimSpace(n.Name) == "" {
		return fmt.Errorf("%w: a term needs a name", ErrInvalidTerm)
	}
	if !n.EndsOn.After(n.StartsOn) {
		return fmt.Errorf("%w: the term must end after it starts", ErrInvalidTerm)
	}
	return nil
}

func (n NewGradeLevel) validate() error {
	if strings.TrimSpace(n.Name) == "" {
		return fmt.Errorf("%w: a class needs a name", ErrInvalidClass)
	}
	return nil
}

func (n NewSection) validate() error {
	if strings.TrimSpace(n.Name) == "" {
		return fmt.Errorf("%w: a section needs a name", ErrInvalidSection)
	}
	if n.Capacity < 0 {
		return fmt.Errorf("%w: capacity cannot be negative", ErrInvalidSection)
	}
	return nil
}

func (n NewSubject) validate() error {
	if strings.TrimSpace(n.Name) == "" {
		return fmt.Errorf("%w: a subject needs a name", ErrInvalidSubject)
	}
	return nil
}
