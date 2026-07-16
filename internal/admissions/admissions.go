// Package admissions models application intake: a prospective student applies, the
// office accepts or rejects, and an accepted applicant is later admitted. It knows
// nothing about HTTP, references the academic spine (classes, students) by id, and
// never mints an account or a student itself — the admit step that creates a student
// is a cross-domain orchestration the coordinator wires; this package only records
// the resulting student id.
package admissions

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Sentinels. internal/httpapi maps each to a status; add a line to
// admissions_errors_test.go in the same commit as a new one.
var (
	ErrNotFound           = errors.New("admissions: not found")
	ErrInvalidApplication = errors.New("admissions: the application is not valid")
	ErrNotPending         = errors.New("admissions: only a pending application can be decided that way")
	ErrInvalidPage        = errors.New("admissions: the page cursor is not valid")
)

// Application status.
const (
	StatusPending  = "pending"
	StatusAccepted = "accepted"
	StatusRejected = "rejected"
	StatusAdmitted = "admitted"
)

// Audit actions.
const (
	ActionSubmitted = "admission.submitted"
	ActionAccepted  = "admission.accepted"
	ActionRejected  = "admission.rejected"
	ActionAdmitted  = "admission.admitted"
)

// Application is one prospective student's intake record.
type Application struct {
	ID            uuid.UUID
	ApplicantName string
	GuardianName  string
	GuardianPhone string
	GuardianEmail string
	GradeLevelID  *uuid.UUID
	DOB           *time.Time
	Status        string
	Note          string
	StudentID     *uuid.UUID
	SubmittedAt   time.Time
	DecidedAt     *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// NewApplication is an application to submit.
type NewApplication struct {
	ApplicantName string
	GuardianName  string
	GuardianPhone string
	GuardianEmail string
	GradeLevelID  *uuid.UUID
	DOB           *time.Time
	Note          string
}

// Filter narrows the intake listing.
type Filter struct {
	Status string
}

func (n *NewApplication) validate() error {
	if n.ApplicantName == "" {
		return fmt.Errorf("%w: name the applicant", ErrInvalidApplication)
	}
	return nil
}
