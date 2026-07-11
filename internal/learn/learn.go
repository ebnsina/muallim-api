// Package learn owns what a learner makes as they study: the private note and
// highlights they keep on a lesson, and the public questions they ask on it.
//
// A note and a highlight are private, so they are answered by id alone. A
// question is shared with everyone studying the course, so whether a reader may
// see or post one follows whether they may read the lesson — checked in the
// query by a join to enrolments, the same shared-schema read the course-wide
// lists already use. The package still imports neither catalog nor enroll: it
// reads their tables, it does not depend on their code.
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

	// ErrEmptyPost is a question or answer with no text. A blank post is not a
	// contribution, and an empty column is not worth a row.
	ErrEmptyPost = errors.New("learn: post is empty")

	// ErrPostTooLong is a question or answer past the length a thread should hold.
	ErrPostTooLong = errors.New("learn: post is too long")

	// ErrQuestionNotFound is a question, or an answer's parent question, that is not
	// there — or one the caller may neither see nor moderate. All read the same.
	ErrQuestionNotFound = errors.New("learn: question not found")

	// ErrAnswerNotFound is an answer another learner asked to delete, or one that
	// was never there. Both read the same: it is not theirs to remove.
	ErrAnswerNotFound = errors.New("learn: answer not found")
)

// MaxPostLength bounds a question or an answer. A paragraph or two, not an essay.
const MaxPostLength = 5_000

// MaxQuestionsPerPage bounds a lesson's discussion read. A busy lesson shows a
// page of threads, not every question ever asked, in one response.
const MaxQuestionsPerPage = 50

// Participant is who is reading or writing in a lesson's discussion.
//
// CanModerate is an authorisation decision made by the transport layer — it maps
// to course:write — never a request parameter. It lets an instructor answer with
// a badge and delete any post, not only their own.
type Participant struct {
	UserID      uuid.UUID
	CanModerate bool
}

// Question is a learner's public question on a lesson, with its answers.
type Question struct {
	ID         uuid.UUID
	LessonID   uuid.UUID
	AuthorID   uuid.UUID
	AuthorName string
	Body       string
	CreatedAt  time.Time

	Answers []Answer
}

// Answer is a reply to a question. ByInstructor is fixed at write time, so a
// badge does not move when a role does.
type Answer struct {
	ID           uuid.UUID
	QuestionID   uuid.UUID
	AuthorID     uuid.UUID
	AuthorName   string
	Body         string
	ByInstructor bool
	CreatedAt    time.Time
}

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
