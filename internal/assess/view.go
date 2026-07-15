package assess

import (
	"slices"
	"strings"

	"github.com/google/uuid"
)

// LearnerQuiz is a quiz as the person taking it sees it.
//
// It is a distinct type from Quiz, and LearnerQuestion from Question, for one
// reason: there is nowhere in it to put an answer. A shared struct with
// `json:"-"` on the secret fields would be one forgotten tag away from handing a
// learner the answer key, and the forgetting would be silent.
type LearnerQuiz struct {
	ID       uuid.UUID
	LessonID uuid.UUID

	Title       string
	Description string

	TimeLimitSeconds int
	MaxAttempts      int
	PassingPercent   int

	Questions []LearnerQuestion

	// TotalPoints is what the quiz is scored out of.
	TotalPoints int
}

// LearnerQuestion is one item, with the answer removed.
type LearnerQuestion struct {
	ID       uuid.UUID
	Type     string
	Prompt   string
	Points   int
	Position int

	// Options are the choices, or the items to put in order. Never their positions
	// and never which is correct.
	Options []LearnerOption

	// Matches are the right-hand sides of a matching question, detached from the
	// options they belong to. Empty for every other type.
	Matches []LearnerMatch

	// Blanks is how many holes a fill-in-the-blanks prompt has. The accepted
	// spellings are not here, obviously; the count is not a secret and a client
	// needs it to render the inputs.
	Blanks int

	// Image is the base picture for a pin, graph, or drawing — what the browser
	// draws the answer on top of. The hotspot regions and expected points that go
	// with it are the answer and are never here.
	Image string `json:"image,omitempty"`
}

// LearnerOption is a choice with no verdict attached.
type LearnerOption struct {
	ID      uuid.UUID
	Content string
}

// LearnerMatch is the right half of a pair, identified by something other than
// the option it belongs to.
type LearnerMatch struct {
	ID      uuid.UUID
	Content string
}

// forLearner strips a quiz down to what the person answering it may see.
//
// Two things are removed beyond the obvious. An ordering question's options are
// re-sorted by id, because their stored positions *are* the answer and shipping
// them in that order would be shipping it. And a matching question's right-hand
// sides are lifted out into their own list, sorted by their own id, so that
// receiving them tells you nothing about which option each belongs to.
//
// Sorting by id rather than shuffling per request: uuid v4 order is arbitrary,
// and a stable order means a learner who reloads the page does not watch the
// options rearrange themselves.
func forLearner(q Quiz, questions []Question) LearnerQuiz {
	view := LearnerQuiz{
		ID:               q.ID,
		LessonID:         q.LessonID,
		Title:            q.Title,
		Description:      q.Description,
		TimeLimitSeconds: q.TimeLimitSeconds,
		MaxAttempts:      q.MaxAttempts,
		PassingPercent:   q.PassingPercent,
		Questions:        make([]LearnerQuestion, 0, len(questions)),
	}

	for _, question := range questions {
		view.TotalPoints += question.Points
		view.Questions = append(view.Questions, questionForLearner(question))
	}
	return view
}

func questionForLearner(q Question) LearnerQuestion {
	view := LearnerQuestion{
		ID:       q.ID,
		Type:     q.Type,
		Prompt:   q.Prompt,
		Points:   q.Points,
		Position: q.Position,
		Blanks:   len(q.Accepted),
	}

	// The base image a pin/graph/draw answer is made on. Never its regions or points.
	if q.Spec != nil {
		view.Image = q.Spec.Image
	}

	// short_answer has one blank and no prose around it, so the count says nothing
	// a client needs. Reporting it would only invite a renderer to draw two boxes
	// for a question that has one.
	if q.Type == TypeShortAnswer {
		view.Blanks = 0
	}

	options := slices.Clone(q.Options)

	switch q.Type {
	case TypeOrdering:
		// The author's order is the answer. Ship them in an order that is not it.
		slices.SortFunc(options, func(a, b Option) int { return strings.Compare(a.ID.String(), b.ID.String()) })

	case TypeMatching:
		matches := make([]LearnerMatch, 0, len(options))
		for _, o := range options {
			matches = append(matches, LearnerMatch{ID: o.MatchID, Content: o.MatchContent})
		}
		// Sorted by their own id: no correlation with the options' order.
		slices.SortFunc(matches, func(a, b LearnerMatch) int { return strings.Compare(a.ID.String(), b.ID.String()) })
		view.Matches = matches
	}

	view.Options = make([]LearnerOption, 0, len(options))
	for _, o := range options {
		view.Options = append(view.Options, LearnerOption{ID: o.ID, Content: o.Content})
	}

	// An essay and a typed answer have nothing to choose from.
	if len(view.Options) == 0 {
		view.Options = nil
	}
	return view
}

// AttemptReview is a graded attempt, as its learner sees it.
//
// It says what they answered, whether it was right, and why — never what the
// right answer was. A quiz that allows a second attempt would otherwise hand out
// the answer key with the first result.
type AttemptReview struct {
	Attempt Attempt
	Items   []ReviewItem
}

// ReviewItem is one question, one answer, one verdict.
type ReviewItem struct {
	QuestionID uuid.UUID
	Prompt     string
	Type       string
	Position   int

	Response Response

	// Graded is false while an essay waits for an instructor.
	Graded    bool
	Correct   bool
	Points    int
	MaxPoints int

	// Explanation is the author's note on the question, released once the attempt
	// has been graded. This is the feature Tutor LMS has never shipped.
	Explanation string

	// Feedback is what an instructor wrote about this particular answer.
	Feedback string
}
