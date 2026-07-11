// Package assess owns quizzes, questions, attempts, and grading.
//
// One invariant governs the whole package: a learner never receives the answer.
// Not the correct option, not the accepted spellings, not the position that makes
// an ordering right. That is enforced by the types — the learner-facing views
// have no field to put an answer in — and not by remembering to omit a JSON tag.
//
// Grading is asynchronous. An attempt is submitted, a job grades it, and the
// result appears when it is ready. LearnDash blocks a request for thirty-five
// seconds to save an essay quiz; this is the whole reason not to.
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
)

// ValidQuestionType reports whether t is a type this system grades.
//
// An unknown type is refused at the door rather than stored. A question nothing
// can grade would sit in an attempt forever, holding it out of `graded`.
func ValidQuestionType(t string) bool {
	switch t {
	case TypeTrueFalse, TypeSingleChoice, TypeMultipleChoice, TypeFillBlanks,
		TypeShortAnswer, TypeOrdering, TypeMatching, TypeOpenEnded, TypeRange:
		return true
	default:
		return false
	}
}

// IsManual reports whether a type needs a human to grade it.
func IsManual(t string) bool { return t == TypeOpenEnded }

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

	// Explanation is shown after the attempt is graded, never before. Tutor LMS
	// still cannot do this; it is the single most requested thing it lacks.
	Explanation string

	// CaseSensitive applies to the typed types. Off by default, because "Paris"
	// and "paris" are the same answer and an author who thinks otherwise should
	// have to say so.
	CaseSensitive bool

	// Accepted holds, for each blank in order, the spellings that count as right.
	// Used by fill_blanks and short_answer; empty for every other type.
	Accepted [][]string

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
