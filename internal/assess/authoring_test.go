package assess

import (
	"errors"
	"testing"
)

func opt(content string, correct bool) NewOption {
	return NewOption{Content: content, IsCorrect: correct}
}

// A question that cannot be answered correctly must not be storable. grade()
// refuses to award it either — that is the net — but the author should learn
// about it while writing, not from a learner who could not pass.
func TestNewQuestionValidation(t *testing.T) {
	t.Parallel()

	good := map[string]NewQuestion{
		"true/false": {
			Type: TypeTrueFalse, Prompt: "Go is compiled.", Points: 1,
			Options: []NewOption{opt("True", true), opt("False", false)},
		},
		"single choice": {
			Type: TypeSingleChoice, Prompt: "Which?", Points: 2,
			Options: []NewOption{opt("A", false), opt("B", true), opt("C", false)},
		},
		"multiple choice": {
			Type: TypeMultipleChoice, Prompt: "Which of these?", Points: 2,
			Options: []NewOption{opt("A", true), opt("B", false), opt("C", true)},
		},
		"ordering": {
			Type: TypeOrdering, Prompt: "Order these.", Points: 1,
			Options: []NewOption{opt("First", false), opt("Second", false)},
		},
		"matching": {
			Type: TypeMatching, Prompt: "Pair these.", Points: 1,
			Options: []NewOption{
				{Content: "France", MatchContent: "Paris"},
				{Content: "Japan", MatchContent: "Tokyo"},
			},
		},
		"short answer": {Type: TypeShortAnswer, Prompt: "Capital?", Points: 1, Accepted: [][]string{{"Paris"}}},
		"fill blanks":  {Type: TypeFillBlanks, Prompt: "__ and __.", Points: 1, Accepted: [][]string{{"a"}, {"b"}}},
		"range":        {Type: TypeRange, Prompt: "Boiling point?", Points: 2, Accepted: [][]string{{"99.5", "100.5"}}},
		"open ended":   {Type: TypeOpenEnded, Prompt: "Discuss.", Points: 10},
		"zero points":  {Type: TypeOpenEnded, Prompt: "Ungraded reflection.", Points: 0},
		"puzzle": {
			Type: TypePuzzle, Prompt: "Assemble these.", Points: 2,
			Options: []NewOption{opt("Piece one", false), opt("Piece two", false)},
		},
		"pin": {
			Type: TypePin, Prompt: "Click the capital.", Points: 3,
			Spec: &Spec{Image: "map.png", Regions: []Region{{X: 10, Y: 10, W: 5, H: 5}}},
		},
		"graph": {
			Type: TypeGraph, Prompt: "Plot y = x.", Points: 4,
			Spec: &Spec{Points: []Point{{X: 1, Y: 1}, {X: 2, Y: 2}}, Tolerance: 0.5},
		},
		"draw":                 {Type: TypeDrawImage, Prompt: "Sketch the cell.", Points: 5},
		"draw with a backdrop": {Type: TypeDrawImage, Prompt: "Trace over this.", Points: 5, Spec: &Spec{Image: "cell.png"}},
		"an explanation": {
			Type: TypeTrueFalse, Prompt: "Go is compiled.", Points: 1, Explanation: "It is.",
			Options: []NewOption{opt("True", true), opt("False", false)},
		},
	}

	for name, question := range good {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := question.validate(); err != nil {
				t.Errorf("a well-formed %s was refused: %v", name, err)
			}
		})
	}

	bad := map[string]NewQuestion{
		"a type nothing grades": {Type: "nonesuch", Prompt: "Draw.", Points: 1},

		"pin with no image":   {Type: TypePin, Prompt: "Click.", Points: 1, Spec: &Spec{Regions: []Region{{X: 0, Y: 0, W: 5, H: 5}}}},
		"pin with no regions": {Type: TypePin, Prompt: "Click.", Points: 1, Spec: &Spec{Image: "map.png"}},
		"pin with no spec":    {Type: TypePin, Prompt: "Click.", Points: 1},
		"pin with a flat region": {
			Type: TypePin, Prompt: "Click.", Points: 1,
			Spec: &Spec{Image: "map.png", Regions: []Region{{X: 0, Y: 0, W: 0, H: 5}}},
		},
		"pin with options":     {Type: TypePin, Prompt: "Click.", Points: 1, Spec: &Spec{Image: "m.png", Regions: []Region{{X: 0, Y: 0, W: 5, H: 5}}}, Options: []NewOption{opt("A", false)}},
		"graph with no points": {Type: TypeGraph, Prompt: "Plot.", Points: 1, Spec: &Spec{Tolerance: 1}},
		"graph with no spec":   {Type: TypeGraph, Prompt: "Plot.", Points: 1},
		"graph with a negative tolerance": {
			Type: TypeGraph, Prompt: "Plot.", Points: 1,
			Spec: &Spec{Points: []Point{{X: 1, Y: 1}}, Tolerance: -1},
		},
		"draw with options": {Type: TypeDrawImage, Prompt: "Sketch.", Points: 1, Options: []NewOption{opt("A", false)}},
		// A spec belongs only to the canvas and coordinate types.
		"a spec on a choice question": {
			Type: TypeSingleChoice, Prompt: "Which?", Points: 1,
			Options: []NewOption{opt("A", true), opt("B", false)},
			Spec:    &Spec{Image: "smuggled.png"},
		},
		"no prompt": {
			Type: TypeTrueFalse, Prompt: "  ", Points: 1,
			Options: []NewOption{opt("True", true), opt("False", false)},
		},
		"negative points": {Type: TypeOpenEnded, Prompt: "Discuss.", Points: -1},

		// Every one of these is a question nobody can get right.
		"single choice with no correct option": {
			Type: TypeSingleChoice, Prompt: "Which?", Points: 1,
			Options: []NewOption{opt("A", false), opt("B", false)},
		},
		"single choice with two correct options": {
			Type: TypeSingleChoice, Prompt: "Which?", Points: 1,
			Options: []NewOption{opt("A", true), opt("B", true)},
		},
		"single choice with one option": {
			Type: TypeSingleChoice, Prompt: "Which?", Points: 1,
			Options: []NewOption{opt("A", true)},
		},
		"true/false with three options": {
			Type: TypeTrueFalse, Prompt: "Which?", Points: 1,
			Options: []NewOption{opt("True", true), opt("False", false), opt("Maybe", false)},
		},
		"multiple choice with no correct option": {
			Type: TypeMultipleChoice, Prompt: "Which?", Points: 1,
			Options: []NewOption{opt("A", false), opt("B", false)},
		},

		// Everybody who ticks everything is right, which is not a question.
		"multiple choice where every option is correct": {
			Type: TypeMultipleChoice, Prompt: "Which?", Points: 1,
			Options: []NewOption{opt("A", true), opt("B", true)},
		},

		"an empty option": {
			Type: TypeSingleChoice, Prompt: "Which?", Points: 1,
			Options: []NewOption{opt("A", true), opt("   ", false)},
		},
		"ordering with one item": {
			Type: TypeOrdering, Prompt: "Order it.", Points: 1,
			Options: []NewOption{opt("Only", false)},
		},

		// The answer to an ordering question is the order. A "correct" item would be
		// an answer the grader never reads, and an author who believed they had set it.
		"ordering with a correct item": {
			Type: TypeOrdering, Prompt: "Order these.", Points: 1,
			Options: []NewOption{opt("First", true), opt("Second", false)},
		},
		"matching with nothing to match": {
			Type: TypeMatching, Prompt: "Pair these.", Points: 1,
			Options: []NewOption{{Content: "France", MatchContent: "Paris"}, {Content: "Japan"}},
		},
		"a match on a question that does not match": {
			Type: TypeSingleChoice, Prompt: "Which?", Points: 1,
			Options: []NewOption{
				{Content: "A", IsCorrect: true, MatchContent: "smuggled"},
				{Content: "B"},
			},
		},

		"short answer with two blanks": {Type: TypeShortAnswer, Prompt: "?", Points: 1, Accepted: [][]string{{"a"}, {"b"}}},
		"short answer with no blank":   {Type: TypeShortAnswer, Prompt: "?", Points: 1},
		"fill blanks with none":        {Type: TypeFillBlanks, Prompt: "?", Points: 1},
		"a blank that accepts nothing": {Type: TypeFillBlanks, Prompt: "__", Points: 1, Accepted: [][]string{{}}},
		"a blank accepting the empty string": {
			Type: TypeFillBlanks, Prompt: "__", Points: 1, Accepted: [][]string{{"  "}},
		},

		// An author who sets accepted spellings on a choice question has set an
		// answer nothing reads.
		"accepted spellings on a choice question": {
			Type: TypeSingleChoice, Prompt: "Which?", Points: 1,
			Options:  []NewOption{opt("A", true), opt("B", false)},
			Accepted: [][]string{{"A"}},
		},
		"an essay with options": {
			Type: TypeOpenEnded, Prompt: "Discuss.", Points: 1,
			Options: []NewOption{opt("A", false), opt("B", false)},
		},
		"range with one bound": {
			Type: TypeRange, Prompt: "How much?", Points: 1, Accepted: [][]string{{"5"}},
		},
		"range with non-numeric bounds": {
			Type: TypeRange, Prompt: "How much?", Points: 1, Accepted: [][]string{{"a", "b"}},
		},
		"range with low above high": {
			Type: TypeRange, Prompt: "How much?", Points: 1, Accepted: [][]string{{"10", "5"}},
		},
		"range with options": {
			Type: TypeRange, Prompt: "How much?", Points: 1,
			Accepted: [][]string{{"1", "2"}}, Options: []NewOption{opt("A", false)},
		},
	}

	for name, question := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := question.validate(); !errors.Is(err, ErrInvalidQuestion) {
				t.Errorf("%s was accepted (err = %v)", name, err)
			}
		})
	}
}

func TestNewQuizValidation(t *testing.T) {
	t.Parallel()

	if err := (NewQuiz{Title: "Fine", PassingPercent: 100}).validate(); err != nil {
		t.Errorf("a well-formed quiz was refused: %v", err)
	}

	bad := map[string]NewQuiz{
		"no title":              {Title: "  "},
		"a bar over a hundred":  {Title: "Q", PassingPercent: 101},
		"a negative bar":        {Title: "Q", PassingPercent: -1},
		"a negative time limit": {Title: "Q", TimeLimitSeconds: -1},
		"negative attempts":     {Title: "Q", MaxAttempts: -1},
	}
	for name, quiz := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := quiz.validate(); !errors.Is(err, ErrInvalidQuiz) {
				t.Errorf("%s was accepted (err = %v)", name, err)
			}
		})
	}

	// A patch says nothing about the fields it omits, and must not invent a zero.
	if err := (QuizPatch{}).validate(); err != nil {
		t.Errorf("an empty patch was refused: %v", err)
	}
	empty := ""
	if err := (QuizPatch{Title: &empty}).validate(); !errors.Is(err, ErrInvalidQuiz) {
		t.Error("a patch may not blank the title")
	}
}
