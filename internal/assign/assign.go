// Package assign owns assignments: work a learner uploads and a person marks.
//
// The bytes never pass through this process. A browser is handed a URL signed by
// the object store and uploads straight to it; afterwards it tells us the key,
// and we ask the store what is really there before writing a row. That ordering
// is the whole design. A row written when the URL was signed points at nothing
// whenever an upload is abandoned, which on a phone on a train is most of the
// time.
//
// It knows nothing about HTTP. It returns its own sentinel errors.
package assign

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Sentinel errors.
var (
	ErrNotFound = errors.New("assign: not found")

	// ErrAssignmentExists means the lesson already has one. A lesson has one.
	ErrAssignmentExists = errors.New("assign: the lesson already has an assignment")

	// ErrInvalidAssignment means the assignment's own settings contradict each other.
	ErrInvalidAssignment = errors.New("assign: the assignment is not valid")

	// ErrInvalidFile means the file cannot be accepted: no name, no bytes, or more
	// bytes than the assignment allows.
	ErrInvalidFile = errors.New("assign: that file cannot be uploaded")

	// ErrTooManyFiles means the submission already holds as many as it may.
	ErrTooManyFiles = errors.New("assign: no more files may be attached")

	// ErrPastDue means the deadline has passed and the assignment does not take
	// late work.
	ErrPastDue = errors.New("assign: the deadline has passed")

	// ErrAlreadySubmitted means the learner has handed it in. A submitted
	// assignment is not a draft, and its files are not theirs to change.
	ErrAlreadySubmitted = errors.New("assign: that submission has already been handed in")

	// ErrNothingToSubmit means the draft holds no files.
	ErrNothingToSubmit = errors.New("assign: attach at least one file before handing in")

	// ErrNotSubmitted means there is nothing to mark yet.
	ErrNotSubmitted = errors.New("assign: that submission has not been handed in")

	// ErrInvalidGrade means the award is not between zero and what the assignment
	// is worth.
	ErrInvalidGrade = errors.New("assign: the award is not within the assignment's points")

	// ErrUploadMissing means the client confirmed a key the object store has never
	// heard of, or one whose bytes do not match what was signed for.
	ErrUploadMissing = errors.New("assign: nothing was uploaded to that key")

	// ErrNotYours means the object key does not belong to this learner's submission.
	// It is deliberately distinct from ErrNotFound inside the domain, and
	// deliberately indistinguishable from it over HTTP.
	ErrNotYours = errors.New("assign: that upload belongs to somebody else")
)

// Audit actions this package emits.
const (
	ActionAssignmentCreated = "assignment.created"
	ActionAssignmentUpdated = "assignment.updated"
	ActionAssignmentDeleted = "assignment.deleted"
	ActionSubmitted         = "submission.handed_in"
	ActionMarked            = "submission.marked"
)

// Submission statuses.
const (
	// StatusDraft: the learner is still attaching files. The only status they may
	// write in.
	StatusDraft = "draft"

	// StatusSubmitted: handed in, waiting for a person.
	StatusSubmitted = "submitted"

	// StatusGraded: final.
	StatusGraded = "graded"
)

// How long a signed URL lives.
//
// An upload URL is a bearer token for one object at one size. Long enough for a
// slow phone to finish a twenty-five megabyte file; short enough that a URL found
// in a browser's history a day later is worth nothing.
const (
	UploadTTL   = 15 * time.Minute
	DownloadTTL = 5 * time.Minute
)

// MaxFilenameLength is what most filesystems hold, in bytes.
const MaxFilenameLength = 255

// Assignment is work attached to a lesson. A lesson has at most one.
type Assignment struct {
	ID       uuid.UUID
	LessonID uuid.UUID

	Title        string
	Instructions string

	Points        int
	PassingPoints int

	MaxFiles int
	MaxBytes int64

	DueAt     *time.Time
	AllowLate bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Submission is one learner's answer to it.
type Submission struct {
	ID           uuid.UUID
	AssignmentID uuid.UUID
	UserID       uuid.UUID

	Status string

	SubmittedAt *time.Time
	GradedAt    *time.Time

	// Points is nil until a person has marked it.
	Points   *int
	Feedback string
	GradedBy *uuid.UUID

	// Late records that it arrived after the deadline, as the deadline stood at
	// the moment it arrived.
	Late bool

	Files []File
}

// Passed reports whether a marked submission cleared the bar.
//
// An unmarked submission has not passed and has not failed. `false` here is not
// "failed"; the caller checks Points first, which is why this is unexported to
// nobody and named for what it says.
func (s Submission) Passed(assignment Assignment) bool {
	return s.Points != nil && *s.Points >= assignment.PassingPoints
}

// File is an object in the store, and what is known about it.
type File struct {
	ID           uuid.UUID
	SubmissionID uuid.UUID

	// ObjectKey names the object. It is a uuid under a prefix this package built,
	// never anything a learner typed.
	ObjectKey string

	Filename    string
	ContentType string
	Bytes       int64
	UploadedAt  time.Time
}

// Author identifies who did something, for the audit trail.
type Author struct {
	UserID    uuid.UUID
	IP        netip.Addr
	UserAgent string
}

/*
objectKey names where a learner's file lives.

Two properties, and both are load-bearing.

It is a uuid. Nothing a learner typed appears anywhere in it, so no filename
they choose can contain a `../`, a leading `/`, a NUL, or a name that means
something to the store. Sanitising a filename into a key is a game of
whack-a-mole against every encoding a browser knows; not putting it there is not.

It is prefixed by the tenant, the assignment, and the learner. That prefix is
recomputed from the authenticated caller when they come back to confirm the
upload, so a key belonging to somebody else's submission is refused before the
object store is ever asked about it — and a leaked key from another workspace
names an object nobody in this one can reach.
*/
func objectKey(tenantID, assignmentID, userID uuid.UUID) string {
	return fmt.Sprintf("%s/%s", keyPrefix(tenantID, assignmentID, userID), uuid.New())
}

func keyPrefix(tenantID, assignmentID, userID uuid.UUID) string {
	return fmt.Sprintf("t/%s/assignments/%s/u/%s", tenantID, assignmentID, userID)
}

/*
cleanFilename makes a name safe to store, to show, and to put in a header.

The name is the learner's, and it is never a path: the key is a uuid. What is
left is presentation — but a `Content-Disposition` carrying a quote or a newline
is a header that ends early and a header that begins, and the one that begins
belongs to whoever uploaded the file.

So: strip the directory (some browsers still send one), drop control characters,
collapse whitespace, and cap the length. An empty result becomes `file`, because
something has to be shown, and a blank name is a name nobody can click.
*/
func cleanFilename(name string) string {
	// Both separators: a Windows browser sends backslashes, and this may run
	// anywhere.
	if index := strings.LastIndexAny(name, `/\`); index >= 0 {
		name = name[index+1:]
	}

	name = strings.Map(func(r rune) rune {
		switch {
		case r == 0 || unicode.IsControl(r):
			return -1
		case unicode.IsSpace(r):
			return ' '
		default:
			return r
		}
	}, name)

	name = strings.TrimSpace(strings.Join(strings.Fields(name), " "))

	// Bytes, not runes: the limit is the filesystem's, and it counts bytes. Trimming
	// there can land mid-rune, so back up until what is left is valid UTF-8 — a
	// half a character is worse than a shorter name.
	if len(name) > MaxFilenameLength {
		name = name[:MaxFilenameLength]
		for len(name) > 0 && !utf8.ValidString(name) {
			name = name[:len(name)-1]
		}
	}

	// `.` and `..` are legal filenames nowhere useful and confusing everywhere.
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	return name
}
