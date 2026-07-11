// Package learn owns what a learner makes as they study — for now, the private
// note they keep on a lesson.
//
// It knows a lesson only by its id. A note references a lesson through a foreign
// key, but the package never reads a course or a curriculum: whether a learner
// may see a lesson is `catalog`'s and `enroll`'s decision, and a note is private
// to the learner whatever the answer. So this package imports neither.
package learn

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// The sentinels. `internal/httpapi` maps them; nothing else does.
var (
	// ErrLessonNotFound is a note written against a lesson that does not exist —
	// the foreign key refuses it, and this is how that refusal reads.
	ErrLessonNotFound = errors.New("learn: lesson not found")

	// ErrNoteTooLong is a note past the length a margin should hold.
	ErrNoteTooLong = errors.New("learn: note is too long")
)

// MaxNoteLength bounds a note. Generous — it is a page of thoughts, not a book —
// but bounded, because an unbounded text column on a per-request write is a way
// to fill a disk.
const MaxNoteLength = 10_000

// Note is a learner's private margin on one lesson.
//
// An empty Body is a real state, not a missing one: a lesson a learner has never
// noted and a note they have cleared are the same to them, and the service treats
// them the same — no row.
type Note struct {
	LessonID  uuid.UUID
	Body      string
	UpdatedAt time.Time
}
