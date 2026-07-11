package assess

import (
	"fmt"
	"strconv"
	"strings"
)

// NewQuiz describes a quiz to attach to a lesson.
type NewQuiz struct {
	Title       string
	Description string

	TimeLimitSeconds int
	MaxAttempts      int
	PassingPercent   int
}

// QuizPatch updates a quiz. A nil field is left alone.
type QuizPatch struct {
	Title            *string
	Description      *string
	TimeLimitSeconds *int
	MaxAttempts      *int
	PassingPercent   *int
}

// NewQuestion describes a question to append to a quiz.
type NewQuestion struct {
	Type   string
	Prompt string
	Points int

	Explanation   string
	CaseSensitive bool

	// Accepted holds one set of acceptable spellings per blank, in order.
	Accepted [][]string

	Options []NewOption
}

// NewOption is a choice, an item to place, or the left half of a pair.
type NewOption struct {
	Content string

	// IsCorrect marks a correct choice. Meaningless for the types that have no
	// choices, and refused there rather than ignored.
	IsCorrect bool

	// MatchContent is the right half of a matching pair.
	MatchContent string
}

func (n NewQuiz) validate() error {
	if strings.TrimSpace(n.Title) == "" {
		return fmt.Errorf("%w: a quiz needs a title", ErrInvalidQuiz)
	}
	if n.TimeLimitSeconds < 0 || n.MaxAttempts < 0 {
		return fmt.Errorf("%w: a limit cannot be negative; use zero for none", ErrInvalidQuiz)
	}
	if n.PassingPercent < 0 || n.PassingPercent > 100 {
		return fmt.Errorf("%w: the passing grade is a percentage", ErrInvalidQuiz)
	}
	return nil
}

func (p QuizPatch) validate() error {
	if p.Title != nil && strings.TrimSpace(*p.Title) == "" {
		return fmt.Errorf("%w: a quiz needs a title", ErrInvalidQuiz)
	}
	if (p.TimeLimitSeconds != nil && *p.TimeLimitSeconds < 0) || (p.MaxAttempts != nil && *p.MaxAttempts < 0) {
		return fmt.Errorf("%w: a limit cannot be negative; use zero for none", ErrInvalidQuiz)
	}
	if p.PassingPercent != nil && (*p.PassingPercent < 0 || *p.PassingPercent > 100) {
		return fmt.Errorf("%w: the passing grade is a percentage", ErrInvalidQuiz)
	}
	return nil
}

// validate refuses a question that cannot be answered correctly.
//
// This is where "a single-choice question with no correct option" is stopped.
// grade() also refuses to award it — denying by default, since it cannot know how
// the row got there — but a question nobody can pass is an authoring mistake, and
// discovering it while writing beats discovering it from a learner.
func (n NewQuestion) validate() error {
	if !ValidQuestionType(n.Type) {
		return fmt.Errorf("%w: %q is not a question type", ErrInvalidQuestion, n.Type)
	}
	if strings.TrimSpace(n.Prompt) == "" {
		return fmt.Errorf("%w: a question needs a prompt", ErrInvalidQuestion)
	}
	if n.Points < 0 {
		return fmt.Errorf("%w: points cannot be negative", ErrInvalidQuestion)
	}

	var correct int
	for _, o := range n.Options {
		if strings.TrimSpace(o.Content) == "" {
			return fmt.Errorf("%w: an option needs content", ErrInvalidQuestion)
		}
		if o.IsCorrect {
			correct++
		}
	}

	switch n.Type {
	case TypeTrueFalse:
		if len(n.Options) != 2 {
			return fmt.Errorf("%w: a true/false question has exactly two options", ErrInvalidQuestion)
		}
		if correct != 1 {
			return fmt.Errorf("%w: a true/false question has exactly one correct option", ErrInvalidQuestion)
		}

	case TypeSingleChoice, TypeImageAnswering:
		if len(n.Options) < 2 {
			return fmt.Errorf("%w: a single-choice question needs at least two options", ErrInvalidQuestion)
		}
		if correct != 1 {
			return fmt.Errorf("%w: a single-choice question has exactly one correct option", ErrInvalidQuestion)
		}

	case TypeMultipleChoice:
		if len(n.Options) < 2 {
			return fmt.Errorf("%w: a multiple-choice question needs at least two options", ErrInvalidQuestion)
		}
		if correct == 0 {
			return fmt.Errorf("%w: a multiple-choice question needs a correct option", ErrInvalidQuestion)
		}
		if correct == len(n.Options) {
			// Not forbidden by any rule of logic, but every learner who ticks
			// everything is right, which is not what the author meant to ask.
			return fmt.Errorf("%w: a multiple-choice question where every option is correct asks nothing", ErrInvalidQuestion)
		}

	case TypeOrdering:
		if len(n.Options) < 2 {
			return fmt.Errorf("%w: an ordering question needs at least two items", ErrInvalidQuestion)
		}
		if correct > 0 {
			return fmt.Errorf("%w: an ordering question's answer is the order, not a correct item", ErrInvalidQuestion)
		}

	case TypeMatching, TypeImageMatching:
		if len(n.Options) < 2 {
			return fmt.Errorf("%w: a matching question needs at least two pairs", ErrInvalidQuestion)
		}
		for _, o := range n.Options {
			if strings.TrimSpace(o.MatchContent) == "" {
				return fmt.Errorf("%w: every matching item needs something to match", ErrInvalidQuestion)
			}
		}

	case TypeShortAnswer:
		if len(n.Accepted) != 1 {
			return fmt.Errorf("%w: a short answer has exactly one blank", ErrInvalidQuestion)
		}

	case TypeFillBlanks:
		if len(n.Accepted) == 0 {
			return fmt.Errorf("%w: a fill-in-the-blanks question needs at least one blank", ErrInvalidQuestion)
		}

	case TypeOpenEnded:
		if len(n.Options) > 0 {
			return fmt.Errorf("%w: an essay has nothing to choose from", ErrInvalidQuestion)
		}

	case TypeRange:
		// One pair of numbers, low then high, and low no greater than high.
		if len(n.Options) > 0 {
			return fmt.Errorf("%w: a range question has nothing to choose from", ErrInvalidQuestion)
		}
		if len(n.Accepted) != 1 || len(n.Accepted[0]) != 2 {
			return fmt.Errorf("%w: a range question needs one low and one high bound", ErrInvalidQuestion)
		}
		lo, err1 := strconv.ParseFloat(strings.TrimSpace(n.Accepted[0][0]), 64)
		hi, err2 := strconv.ParseFloat(strings.TrimSpace(n.Accepted[0][1]), 64)
		if err1 != nil || err2 != nil {
			return fmt.Errorf("%w: a range's bounds must be numbers", ErrInvalidQuestion)
		}
		if lo > hi {
			return fmt.Errorf("%w: a range's low bound cannot exceed its high bound", ErrInvalidQuestion)
		}
	}

	// The typed types need spellings; the others must not carry any, or an author
	// would believe they had set an answer that nothing reads.
	switch n.Type {
	case TypeShortAnswer, TypeFillBlanks:
		for _, blank := range n.Accepted {
			if len(blank) == 0 {
				return fmt.Errorf("%w: every blank needs at least one accepted answer", ErrInvalidQuestion)
			}
			for _, spelling := range blank {
				if strings.TrimSpace(spelling) == "" {
					return fmt.Errorf("%w: an accepted answer cannot be blank", ErrInvalidQuestion)
				}
			}
		}
	case TypeRange:
		// Its bounds are validated above; it is allowed to carry them.
	default:
		if len(n.Accepted) > 0 {
			return fmt.Errorf("%w: a %s question has no blanks to accept answers for", ErrInvalidQuestion, n.Type)
		}
		if n.Type != TypeMatching && n.Type != TypeImageMatching {
			for _, o := range n.Options {
				if strings.TrimSpace(o.MatchContent) != "" {
					return fmt.Errorf("%w: only a matching question has matches", ErrInvalidQuestion)
				}
			}
		}
	}

	if n.Type != TypeOpenEnded && len(n.Options) == 0 && len(n.Accepted) == 0 {
		return fmt.Errorf("%w: a %s question needs something to answer", ErrInvalidQuestion, n.Type)
	}
	return nil
}
