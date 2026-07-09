package enroll

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrPrerequisitesUnmet is returned when a learner tries to enrol on a course
// whose prerequisites they have not finished.
//
// It is a 403, not a 404. The course is published and plainly visible; the answer
// is "finish those first", and a 404 there would be a lie that hides the reason.
var ErrPrerequisitesUnmet = errors.New("enroll: the prerequisite courses are not complete")

// MissingCourse is a prerequisite a learner has not finished. Named so a client
// can say which, rather than "some course".
type MissingCourse struct {
	Slug  string
	Title string
}

// PrerequisiteRepository answers which prerequisites a learner still owes.
//
// The prerequisite graph is written by the catalog package and read here. Two
// packages, one table, no import between them — a domain package never depends
// on a sibling.
type PrerequisiteRepository interface {
	MissingPrerequisites(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) ([]MissingCourse, error)
}

// UnmetPrerequisites carries the courses that stand in a learner's way.
//
// It wraps ErrPrerequisitesUnmet, so a caller that only wants the status code
// uses errors.Is, and a caller that wants to name the courses uses errors.As.
// Neither has to parse a message.
type UnmetPrerequisites struct {
	Missing []MissingCourse
}

func (e *UnmetPrerequisites) Error() string { return ErrPrerequisitesUnmet.Error() }

// Unwrap makes errors.Is(err, ErrPrerequisitesUnmet) true.
func (e *UnmetPrerequisites) Unwrap() error { return ErrPrerequisitesUnmet }
