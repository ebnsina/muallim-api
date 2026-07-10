package assess

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func authoredQuiz() (Quiz, []Question) {
	quiz := Quiz{
		ID: uuid.New(), LessonID: uuid.New(),
		Title: "Chapter one", PassingPercent: 60,
	}

	questions := []Question{
		{
			ID: uuid.New(), Type: TypeSingleChoice, Prompt: "Which?", Points: 2, Position: 0,
			Explanation: "Because B.",
			Options: []Option{
				{ID: uuid.New(), Content: "A", Position: 0, IsCorrect: false},
				{ID: uuid.New(), Content: "B", Position: 1, IsCorrect: true},
			},
		},
		{
			ID: uuid.New(), Type: TypeShortAnswer, Prompt: "Roman name for Paris?", Points: 1, Position: 1,
			Accepted: [][]string{{"Lutetia"}},
		},
		{
			ID: uuid.New(), Type: TypeFillBlanks, Prompt: "2+2 is __ and __ is the capital.", Points: 3, Position: 2,
			Accepted: [][]string{{"4", "four"}, {"Lutetia"}},
		},
		{
			ID: uuid.New(), Type: TypeOrdering, Prompt: "Order these.", Points: 2, Position: 3,
			Options: []Option{
				{ID: uuid.MustParse("ffffffff-0000-0000-0000-000000000000"), Content: "First", Position: 0},
				{ID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Content: "Second", Position: 1},
			},
		},
		{
			ID: uuid.New(), Type: TypeMatching, Prompt: "Pair these.", Points: 2, Position: 4,
			Options: []Option{
				{
					ID: uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000000"), Content: "France", Position: 0,
					MatchID: uuid.MustParse("ffffffff-1111-0000-0000-000000000000"), MatchContent: "Paris",
				},
				{
					ID: uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000000"), Content: "Japan", Position: 1,
					MatchID: uuid.MustParse("11111111-1111-0000-0000-000000000000"), MatchContent: "Tokyo",
				},
			},
		},
		{ID: uuid.New(), Type: TypeOpenEnded, Prompt: "Discuss.", Points: 10, Position: 5},
	}

	return quiz, questions
}

// The invariant the package exists to keep. Serialise the learner's view and go
// looking for anything an answer could hide in.
func TestTheLearnerViewCarriesNoAnswer(t *testing.T) {
	t.Parallel()

	quiz, questions := authoredQuiz()
	view := forLearner(quiz, questions)

	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(encoded)

	// The accepted spellings, and the explanation, which is released only once the
	// attempt has been graded. "Paris" is deliberately not among them: it is the
	// visible half of a matching pair, and a test that forbade it would be testing
	// the fixture rather than the view.
	for _, secret := range []string{"Lutetia", "four", "Because B.", "IsCorrect", "Accepted", "Explanation"} {
		if strings.Contains(body, secret) {
			t.Errorf("the learner's view of the quiz contains %q:\n%s", secret, body)
		}
	}

	// No field of the view, at any depth, is named after a secret. The strings
	// above catch today's leak; this catches the one somebody adds next year.
	forbidden := []string{"correct", "accepted", "answer", "explanation", "position"}
	walkFields(t, reflect.TypeFor[LearnerQuiz](), func(path, name string) {
		lower := strings.ToLower(name)
		for _, word := range forbidden {
			if strings.Contains(lower, word) {
				// Position on the question is the question's own order in the quiz,
				// which is what a client renders it in. An *option's* position is the
				// answer to an ordering question, and there is no such field.
				if name == "Position" && path == "LearnerQuiz.Questions.LearnerQuestion" {
					continue
				}
				t.Errorf("%s.%s: a learner's view has no business with %q", path, name, word)
			}
		}
	})

	// Sanity: the view is not empty, so the absence above means something.
	if len(view.Questions) != len(questions) || view.TotalPoints != 20 {
		t.Fatalf("view has %d questions worth %d, want %d worth 20",
			len(view.Questions), view.TotalPoints, len(questions))
	}
}

func walkFields(t *testing.T, typ reflect.Type, visit func(path, name string)) {
	t.Helper()

	for typ.Kind() == reflect.Slice || typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return
	}

	for field := range typ.Fields() {
		visit(typ.Name(), field.Name)

		child := field.Type
		for child.Kind() == reflect.Slice || child.Kind() == reflect.Pointer {
			child = child.Elem()
		}
		if child.Kind() == reflect.Struct && child != typ {
			walkFields(t, child, func(path, name string) {
				visit(typ.Name()+"."+field.Name+"."+path, name)
			})
		}
	}
}

// An ordering question's answer *is* the option order. Sending the options in the
// author's order would send the answer.
func TestOrderingOptionsAreNotSentInTheAuthorsOrder(t *testing.T) {
	t.Parallel()

	quiz, questions := authoredQuiz()
	view := forLearner(quiz, questions)

	var ordering LearnerQuestion
	for _, q := range view.Questions {
		if q.Type == TypeOrdering {
			ordering = q
		}
	}

	if len(ordering.Options) != 2 {
		t.Fatalf("ordering question has %d options", len(ordering.Options))
	}

	// The fixture's first-positioned option has the id that sorts last, so the
	// learner sees "Second" before "First". A view that shipped position order
	// would have them the other way round.
	if ordering.Options[0].Content != "Second" {
		t.Errorf("options came out as %q, %q — that is the author's order, which is the answer",
			ordering.Options[0].Content, ordering.Options[1].Content)
	}

	// The order is stable: reloading the page must not rearrange the question.
	again := forLearner(quiz, questions)
	if !slices.Equal(ids(ordering.Options), ids(again.Questions[3].Options)) {
		t.Error("the option order changed between two views of the same quiz")
	}
}

func ids(options []LearnerOption) []uuid.UUID {
	out := make([]uuid.UUID, len(options))
	for i, o := range options {
		out[i] = o.ID
	}
	return out
}

// A matching question is answerable only because the matches arrive detached from
// the options. If the two lists lined up, the question would answer itself.
func TestMatchingSendsTheMatchesDetached(t *testing.T) {
	t.Parallel()

	quiz, questions := authoredQuiz()
	view := forLearner(quiz, questions)

	var matching LearnerQuestion
	for _, q := range view.Questions {
		if q.Type == TypeMatching {
			matching = q
		}
	}

	if len(matching.Options) != 2 || len(matching.Matches) != 2 {
		t.Fatalf("matching question: %d options, %d matches", len(matching.Options), len(matching.Matches))
	}

	// The fixture pairs France→Paris and Japan→Tokyo, and Tokyo's match id sorts
	// before Paris's. Index i of one list must not be the answer to index i of the
	// other.
	if matching.Options[0].Content == "France" && matching.Matches[0].Content == "Paris" {
		t.Error("the matches arrived in the options' order, which is the answer")
	}

	// The option ids and the match ids share no value, so knowing one tells you
	// nothing about the other.
	for _, o := range matching.Options {
		for _, m := range matching.Matches {
			if o.ID == m.ID {
				t.Errorf("option %s and a match share an id", o.ID)
			}
		}
	}
}

// The count of blanks is not a secret and a client needs it. A short answer has
// one hole and no prose around it, so reporting a count would only make a
// renderer draw the wrong thing.
func TestBlanksAreCountedOnlyWhereTheyAreDrawn(t *testing.T) {
	t.Parallel()

	quiz, questions := authoredQuiz()
	view := forLearner(quiz, questions)

	byType := map[string]LearnerQuestion{}
	for _, q := range view.Questions {
		byType[q.Type] = q
	}

	if got := byType[TypeFillBlanks].Blanks; got != 2 {
		t.Errorf("fill_blanks reports %d blanks, want 2", got)
	}
	if got := byType[TypeShortAnswer].Blanks; got != 0 {
		t.Errorf("short_answer reports %d blanks, want 0", got)
	}
	if byType[TypeOpenEnded].Options != nil {
		t.Error("an essay has nothing to choose from")
	}
}
