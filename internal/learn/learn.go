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

	// ErrHighlightNotFound is a highlight another learner asked to change or delete,
	// or one that was never there. Both read the same: it is not theirs to touch.
	ErrHighlightNotFound = errors.New("learn: highlight not found")

	// ErrInvalidHighlight is a mark that could not have come from a real selection —
	// an empty quote, or a range that starts before the text or ends before it
	// starts.
	ErrInvalidHighlight = errors.New("learn: invalid highlight")
)

// MaxNoteLength bounds a note. Generous — it is a page of thoughts, not a book —
// but bounded, because an unbounded text column on a per-request write is a way
// to fill a disk.
const MaxNoteLength = 10_000

// MaxQuoteLength bounds the text a single highlight may cover. A learner marks a
// sentence or a paragraph, not a chapter.
const MaxQuoteLength = 2_000

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

// Highlight is a passage a learner marked in a lesson, with an optional note.
//
// Start and End are character offsets into the lesson's text — End exclusive —
// and Quote is the text that was under them when the mark was made. A client
// places the mark by the offsets while the lesson is unchanged, and recognises
// the passage by the quote after it is not.
type Highlight struct {
	ID        uuid.UUID
	LessonID  uuid.UUID
	Quote     string
	Note      string
	Start     int
	End       int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// validate refuses a mark that could not have come from a real selection, before
// it reaches the database's own CHECK. Trimming is not applied to the quote: the
// text under the offsets is the text under the offsets, whitespace and all.
func (h Highlight) validate() error {
	if h.Quote == "" || h.Start < 0 || h.End <= h.Start {
		return ErrInvalidHighlight
	}
	if len(h.Quote) > MaxQuoteLength {
		return ErrInvalidHighlight
	}
	if len(h.Note) > MaxNoteLength {
		return ErrNoteTooLong
	}
	return nil
}
