// Package assess owns quizzes, questions, attempts, and grading.
//
// One invariant governs the whole package: a learner never receives the answer.
// Not the correct option, not the accepted spellings, not the position that makes
// an ordering right. That is enforced by the types — the learner-facing views
// have no field to put an answer in — and not by remembering to omit a JSON tag.
//
// Grading is asynchronous. An attempt is submitted, a job grades it, and the
// result appears when it is ready. A synchronous grader holds the request open
// for as long as grading takes — tens of seconds for an essay quiz; this is the
// whole reason not to.
//
// It knows nothing about HTTP. It returns its own sentinel errors.
package assess

import (
	"errors"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors.
var (
	ErrNotFound = errors.New("assess: not found")

	// ErrInvalidPage is an opaque bank cursor that did not decode.
	ErrInvalidPage = errors.New("assess: invalid page cursor")

	// ErrInvalidQuestion means an author described a question that cannot be
	// answered — a single-choice question with two correct options, an ordering
	// question with one option to order.
	ErrInvalidQuestion = errors.New("assess: the question is not valid")

	// ErrInvalidQuiz means the quiz's own settings contradict each other.
	ErrInvalidQuiz = errors.New("assess: the quiz is not valid")

	// ErrQuizExists means the lesson already has a quiz. A lesson has one.
	ErrQuizExists = errors.New("assess: the lesson already has a quiz")

	// ErrAttemptsExhausted means the learner has used every attempt the quiz allows.
	ErrAttemptsExhausted = errors.New("assess: no attempts remain")

	// ErrAttemptClosed means the attempt has been submitted and can no longer be
	// answered. Grading is not a state a learner may write into.
	ErrAttemptClosed = errors.New("assess: the attempt is no longer open")

	// ErrAttemptExpired means the time limit ran out before the answer arrived.
	ErrAttemptExpired = errors.New("assess: the attempt's time limit has passed")

	// ErrNotYourAttempt means the attempt belongs to somebody else. It is
	// deliberately distinct from ErrNotFound inside the domain, and deliberately
	// indistinguishable from it over HTTP.
	ErrNotYourAttempt = errors.New("assess: the attempt belongs to another learner")

	// ErrEmptyQuiz means the quiz has no questions, so an attempt at it would be
	// graded out of zero.
	ErrEmptyQuiz = errors.New("assess: a quiz needs at least one question")

	// ErrAttemptInProgress means the learner already has a live attempt. Starting
	// is idempotent, so callers hand the existing one back rather than surfacing
	// this; it exists so the repository can say what the database refused.
	ErrAttemptInProgress = errors.New("assess: an attempt is already in progress")

	// ErrIncompleteOrder means a submitted order did not name every sibling
	// exactly once, so it is refused rather than half-applied.
	ErrIncompleteOrder = errors.New("assess: the order must list every question exactly once")

	// ErrNotDrawQuestion means an upload was asked for a question that takes no
	// drawing. Only draw_image answers are uploaded.
	ErrNotDrawQuestion = errors.New("assess: that question takes no uploaded drawing")

	// ErrNoStore means this deployment has no object store, so a drawing has nowhere
	// to go. A workspace without a bucket cannot use draw_image questions.
	ErrNoStore = errors.New("assess: no object store is configured")

	// ErrInvalidUpload means an upload's size or key was refused: a key outside the
	// learner's own prefix is somebody else's object and is never recorded.
	ErrInvalidUpload = errors.New("assess: the upload is not valid")
)

// MaxDrawingBytes bounds an uploaded drawing. A canvas PNG is small; this is room
// for a large one and a wall against a bucket-filling upload.
const MaxDrawingBytes = 8 << 20

// How long a signed drawing URL lives: long enough to finish an upload on a slow
// phone, short enough that a stale URL is worth nothing.
const (
	drawUploadTTL   = 15 * time.Minute
	drawDownloadTTL = 5 * time.Minute
)

// Audit actions this package emits.
const (
	ActionQuizCreated       = "quiz.created"
	ActionQuizUpdated       = "quiz.updated"
	ActionQuizDeleted       = "quiz.deleted"
	ActionQuestionCreated   = "question.created"
	ActionQuestionUpdated   = "question.updated"
	ActionQuestionDeleted   = "question.deleted"
	ActionAttemptStarted    = "attempt.started"
	ActionAttemptSubmitted  = "attempt.submitted"
	ActionAttemptGraded     = "attempt.graded"
	ActionBankQuestionSaved = "bank_question.saved"
)

// Question types.
//
// Each names a distinct grading mechanism, not a distinct widget. Image matching
// grades exactly as matching does, and image answering exactly as single choice;
// they differ in what a browser draws, which is not this package's business.
const (
	// TypeTrueFalse is a single choice between two options.
	TypeTrueFalse = "true_false"

	// TypeSingleChoice has exactly one correct option.
	TypeSingleChoice = "single_choice"

	// TypeMultipleChoice has one or more, and the learner must name them all.
	TypeMultipleChoice = "multiple_choice"

	// TypeFillBlanks has one accepted-answer set per blank, in order.
	TypeFillBlanks = "fill_blanks"

	// TypeShortAnswer is one blank with no surrounding prose.
	TypeShortAnswer = "short_answer"

	// TypeOrdering asks for the options in the author's order.
	TypeOrdering = "ordering"

	// TypeMatching pairs each option with its match.
	TypeMatching = "matching"

	// TypeOpenEnded is an essay. No machine grades it.
	TypeOpenEnded = "open_ended"

	// TypeRange auto-accepts a number within the author's bounds, stored as the
	// single pair Accepted[0] = [min, max].
	TypeRange = "range"

	// TypeImageAnswering grades exactly as single choice; each option's Content is
	// an image URL the browser draws instead of text.
	TypeImageAnswering = "image_answering"

	// TypeImageMatching grades exactly as matching; the item and its match are
	// image URLs. The grading mechanism is the same, only the widget differs.
	TypeImageMatching = "image_matching"

	// TypePuzzle grades exactly as ordering: its pieces are options with the
	// author's positions, arranged by the learner. Only the browser widget differs.
	TypePuzzle = "puzzle"

	// TypePin places a marker on an image; correct when the point lands inside a
	// hotspot region the author drew. The image and regions live in Spec.
	TypePin = "pin"

	// TypeGraph plots points on a plane; correct when every expected point in Spec
	// is hit within Spec.Tolerance, and no extra point is plotted.
	TypeGraph = "graph"

	// TypeDrawImage is a freehand drawing over an optional backdrop. No machine
	// grades it: the learner's image is uploaded and an instructor marks it.
	TypeDrawImage = "draw_image"
)

// ValidQuestionType reports whether t is a type this system grades.
//
// An unknown type is refused at the door rather than stored. A question nothing
// can grade would sit in an attempt forever, holding it out of `graded`.
func ValidQuestionType(t string) bool {
	switch t {
	case TypeTrueFalse, TypeSingleChoice, TypeMultipleChoice, TypeFillBlanks,
		TypeShortAnswer, TypeOrdering, TypeMatching, TypeOpenEnded, TypeRange,
		TypeImageAnswering, TypeImageMatching,
		TypePuzzle, TypePin, TypeGraph, TypeDrawImage:
		return true
	default:
		return false
	}
}

// IsManual reports whether a type needs a human to grade it.
func IsManual(t string) bool { return t == TypeOpenEnded || t == TypeDrawImage }

// Spec carries a type's extra configuration that neither options nor accepted
// spellings can hold: a pin's image and hotspot regions, a graph's expected points
// and tolerance, a drawing's backdrop. It is nil for every type that needs none,
// and its answer-bearing halves (Regions, Points) never leave the domain in a
// learner view — see view.go.
type Spec struct {
	// Image is the base picture a pin is placed on or a drawing is made over.
	Image string `json:"image,omitempty"`

	// Regions are the correct hotspot areas for a pin; a click inside any counts.
	Regions []Region `json:"regions,omitempty"`

	// Points are the coordinates a graph answer must hit, each within Tolerance.
	Points []Point `json:"points,omitempty"`

	// Tolerance is how far a graph point may fall from an expected one and still
	// count. Zero demands an exact hit.
	Tolerance float64 `json:"tolerance,omitempty"`
}

// Region is a rectangle on the base image, in the image's own coordinate space.
type Region struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// Point is a coordinate a learner clicked (pin) or plotted (graph).
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Attempt statuses.
const (
	// StatusInProgress: the learner is answering. The only status they may write in.
	StatusInProgress = "in_progress"

	// StatusGrading: submitted, and a job has been enqueued in the same
	// transaction. Nothing reads a score yet.
	StatusGrading = "grading"

	// StatusAwaitingReview: every machine-gradable question is graded, and an essay
	// is waiting for an instructor. There is a score, and it is not yet final, so
	// Passed is nil.
	StatusAwaitingReview = "awaiting_review"

	// StatusGraded: final.
	StatusGraded = "graded"
)

// Quiz is the assessment attached to a lesson. A lesson has at most one.
type Quiz struct {
	ID       uuid.UUID
	LessonID uuid.UUID
	CourseID uuid.UUID

	Title       string
	Description string

	// TimeLimitSeconds bounds a single attempt. Zero means no limit.
	TimeLimitSeconds int

	// MaxAttempts bounds how many times a learner may try. Zero means unlimited.
	MaxAttempts int

	// PassingPercent is the share of the available points needed to pass, 0–100.
	PassingPercent int

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Question is one item of a quiz, as an author wrote it — answers included.
//
// This type never leaves the domain in a response. LearnerQuestion is what a
// learner sees, and it has nowhere to put an answer.
type Question struct {
	ID     uuid.UUID
	QuizID uuid.UUID

	Type     string
	Prompt   string
	Points   int
	Position int

	// Explanation is shown after the attempt is graded, never before — a commonly
	// requested feature that many quiz tools still lack.
	Explanation string

	// CaseSensitive applies to the typed types. Off by default, because "Paris"
	// and "paris" are the same answer and an author who thinks otherwise should
	// have to say so.
	CaseSensitive bool

	// Accepted holds, for each blank in order, the spellings that count as right.
	// Used by fill_blanks and short_answer; empty for every other type.
	Accepted [][]string

	// Spec is the pin/graph/draw configuration; nil for every other type.
	Spec *Spec

	Options []Option
}

// Option is a choice, an item to order, or half of a pair.
type Option struct {
	ID         uuid.UUID
	QuestionID uuid.UUID

	Content string

	// Position is the author's order. For an ordering question it *is* the answer,
	// so it is never sent to a learner.
	Position int

	// IsCorrect marks a correct choice. Never sent to a learner.
	IsCorrect bool

	// MatchID and MatchContent are the right-hand side of a matching pair. The
	// content is shown; which option it belongs to is the answer, so the two are
	// delivered as separate lists and only MatchID connects them.
	MatchID      uuid.UUID
	MatchContent string
}

// Attempt is one learner's run at a quiz.
type Attempt struct {
	ID     uuid.UUID
	QuizID uuid.UUID
	UserID uuid.UUID

	// Number counts from one, per learner per quiz.
	Number int

	Status string

	StartedAt   time.Time
	SubmittedAt *time.Time
	GradedAt    *time.Time

	// ExpiresAt is StartedAt plus the quiz's time limit, or nil when there is none.
	// Computed once, when the attempt starts, so that changing the limit afterwards
	// cannot shorten an attempt already under way.
	ExpiresAt *time.Time

	// Points and MaxPoints are populated once grading has run. MaxPoints is the sum
	// of the quiz's question points, recorded on the attempt so that editing the
	// quiz later cannot silently restate an old result.
	Points    int
	MaxPoints int

	// Passed is nil until every question has a grade — an essay awaiting review
	// leaves it nil, because a pass is not a thing to guess at.
	Passed *bool
}

// Percent returns the attempt's score as a whole percentage of the points
// available, rounded down. Zero when nothing is available to score.
func (a Attempt) Percent() int {
	if a.MaxPoints <= 0 {
		return 0
	}
	return a.Points * 100 / a.MaxPoints
}

// Answer is a learner's response to one question, with whatever grade it has.
type Answer struct {
	ID         uuid.UUID
	AttemptID  uuid.UUID
	QuestionID uuid.UUID

	Response Response

	// Graded is false until something has judged this answer — a job for the
	// objective types, a person for an essay.
	Graded  bool
	Correct bool
	Points  int

	// Feedback is what an instructor wrote when grading by hand.
	Feedback string
}

// Author identifies who did something, for the audit trail.
type Author struct {
	UserID    uuid.UUID
	IP        netip.Addr
	UserAgent string
}
