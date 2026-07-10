package assess

import (
	"testing"

	"github.com/google/uuid"
)

// Stable ids, so a table can name them.
var (
	optA = uuid.MustParse("00000000-0000-0000-0000-0000000000a1")
	optB = uuid.MustParse("00000000-0000-0000-0000-0000000000b2")
	optC = uuid.MustParse("00000000-0000-0000-0000-0000000000c3")

	matA = uuid.MustParse("00000000-0000-0000-0000-0000000000f1")
	matB = uuid.MustParse("00000000-0000-0000-0000-0000000000f2")

	stranger = uuid.MustParse("00000000-0000-0000-0000-0000000000ff")
)

func choice(id uuid.UUID, correct bool, position int) Option {
	return Option{ID: id, IsCorrect: correct, Position: position}
}

// The whole grading contract, enumerated. Every type, right and wrong, and the
// shapes a hostile or careless client can send: an empty response, the wrong
// field, a duplicate, an id from another question.
func TestGrade(t *testing.T) {
	t.Parallel()

	trueFalse := Question{
		Type: TypeTrueFalse, Points: 1,
		Options: []Option{choice(optA, true, 0), choice(optB, false, 1)},
	}
	single := Question{
		Type: TypeSingleChoice, Points: 3,
		Options: []Option{choice(optA, false, 0), choice(optB, true, 1), choice(optC, false, 2)},
	}
	multiple := Question{
		Type: TypeMultipleChoice, Points: 2,
		Options: []Option{choice(optA, true, 0), choice(optB, false, 1), choice(optC, true, 2)},
	}
	short := Question{
		Type: TypeShortAnswer, Points: 1,
		Accepted: [][]string{{"Paris", "Paris, France"}},
	}
	shortStrict := Question{
		Type: TypeShortAnswer, Points: 1, CaseSensitive: true,
		Accepted: [][]string{{"Paris"}},
	}
	blanks := Question{
		Type: TypeFillBlanks, Points: 4,
		Accepted: [][]string{{"4", "four"}, {"Paris"}},
	}
	ordering := Question{
		Type: TypeOrdering, Points: 2,
		// Deliberately not in position order: the author's order is the position
		// column, not the order the rows happen to arrive in.
		Options: []Option{choice(optC, false, 2), choice(optA, false, 0), choice(optB, false, 1)},
	}
	matching := Question{
		Type: TypeMatching, Points: 2,
		Options: []Option{
			{ID: optA, MatchID: matA},
			{ID: optB, MatchID: matB},
		},
	}
	essay := Question{Type: TypeOpenEnded, Points: 10}

	tests := []struct {
		name     string
		question Question
		response Response
		want     Verdict
	}{
		// True/false and single choice.
		{"true/false right", trueFalse, Response{Choices: []uuid.UUID{optA}}, Verdict{Correct: true, Points: 1}},
		{"true/false wrong", trueFalse, Response{Choices: []uuid.UUID{optB}}, Verdict{}},
		{"single choice right", single, Response{Choices: []uuid.UUID{optB}}, Verdict{Correct: true, Points: 3}},
		{"single choice wrong", single, Response{Choices: []uuid.UUID{optA}}, Verdict{}},
		{
			// Naming every option is not a way to be right about one of them.
			"single choice cannot be answered with two",
			single, Response{Choices: []uuid.UUID{optA, optB}}, Verdict{},
		},
		{"single choice with an id from elsewhere", single, Response{Choices: []uuid.UUID{stranger}}, Verdict{}},
		{"single choice unanswered", single, Response{}, Verdict{}},
		{
			// An author should not be able to save this. If one exists, it is
			// unanswerable rather than always right.
			"single choice with no correct option",
			Question{Type: TypeSingleChoice, Points: 1, Options: []Option{choice(optA, false, 0)}},
			Response{Choices: []uuid.UUID{optA}}, Verdict{},
		},
		{
			"single choice with two correct options",
			Question{Type: TypeSingleChoice, Points: 1, Options: []Option{choice(optA, true, 0), choice(optB, true, 1)}},
			Response{Choices: []uuid.UUID{optA}}, Verdict{},
		},

		// Multiple choice: the exact set, and credit is all-or-nothing.
		{"multiple choice right", multiple, Response{Choices: []uuid.UUID{optA, optC}}, Verdict{Correct: true, Points: 2}},
		{"multiple choice in any order", multiple, Response{Choices: []uuid.UUID{optC, optA}}, Verdict{Correct: true, Points: 2}},
		{"multiple choice missing one", multiple, Response{Choices: []uuid.UUID{optA}}, Verdict{}},
		{"multiple choice with a wrong one included", multiple, Response{Choices: []uuid.UUID{optA, optB, optC}}, Verdict{}},
		{
			// Saying the same thing twice is saying it once. Were duplicates counted,
			// [A, A] would have the right length and the wrong content.
			"multiple choice with a duplicate",
			multiple, Response{Choices: []uuid.UUID{optA, optA, optC}}, Verdict{Correct: true, Points: 2},
		},
		{
			"multiple choice padded to the right length with duplicates",
			multiple, Response{Choices: []uuid.UUID{optA, optA}}, Verdict{},
		},
		{"multiple choice unanswered", multiple, Response{}, Verdict{}},

		// Typed answers.
		{"short answer right", short, Response{Text: "Paris"}, Verdict{Correct: true, Points: 1}},
		{"short answer folded case", short, Response{Text: "  pARIS "}, Verdict{Correct: true, Points: 1}},
		{"short answer collapses inner whitespace", short, Response{Text: "Paris,   France"}, Verdict{Correct: true, Points: 1}},
		{"short answer second spelling", short, Response{Text: "paris, france"}, Verdict{Correct: true, Points: 1}},
		{"short answer wrong", short, Response{Text: "Lyon"}, Verdict{}},
		{"short answer empty", short, Response{Text: "   "}, Verdict{}},
		{"case-sensitive short answer right", shortStrict, Response{Text: "Paris"}, Verdict{Correct: true, Points: 1}},
		{"case-sensitive short answer wrong case", shortStrict, Response{Text: "paris"}, Verdict{}},

		{"fill in the blanks right", blanks, Response{Blanks: []string{"four", "Paris"}}, Verdict{Correct: true, Points: 4}},
		{"fill in the blanks one wrong", blanks, Response{Blanks: []string{"five", "Paris"}}, Verdict{}},
		{
			// All-or-nothing: two of two, or nothing.
			"fill in the blanks one missing",
			blanks, Response{Blanks: []string{"four"}}, Verdict{},
		},
		{"fill in the blanks one empty", blanks, Response{Blanks: []string{"four", "  "}}, Verdict{}},
		{"fill in the blanks too many", blanks, Response{Blanks: []string{"four", "Paris", "extra"}}, Verdict{}},
		{
			// The blanks are ordered. Right answers in the wrong holes are wrong.
			"fill in the blanks transposed",
			Question{Type: TypeFillBlanks, Points: 1, Accepted: [][]string{{"a"}, {"b"}}},
			Response{Blanks: []string{"b", "a"}}, Verdict{},
		},
		{"a typed answer sent in the wrong field", short, Response{Blanks: []string{"Paris"}}, Verdict{}},

		// Ordering.
		{"ordering right", ordering, Response{Order: []uuid.UUID{optA, optB, optC}}, Verdict{Correct: true, Points: 2}},
		{"ordering wrong", ordering, Response{Order: []uuid.UUID{optB, optA, optC}}, Verdict{}},
		{"ordering incomplete", ordering, Response{Order: []uuid.UUID{optA, optB}}, Verdict{}},
		{"ordering with a repeat", ordering, Response{Order: []uuid.UUID{optA, optA, optC}}, Verdict{}},
		{"ordering unanswered", ordering, Response{}, Verdict{}},

		// Matching.
		{
			"matching right", matching,
			Response{Pairs: map[uuid.UUID]uuid.UUID{optA: matA, optB: matB}},
			Verdict{Correct: true, Points: 2},
		},
		{
			"matching swapped", matching,
			Response{Pairs: map[uuid.UUID]uuid.UUID{optA: matB, optB: matA}},
			Verdict{},
		},
		{
			"matching incomplete", matching,
			Response{Pairs: map[uuid.UUID]uuid.UUID{optA: matA}},
			Verdict{},
		},
		{
			// The right pairs plus a spurious one is not the right answer.
			"matching with an extra pair", matching,
			Response{Pairs: map[uuid.UUID]uuid.UUID{optA: matA, optB: matB, stranger: matA}},
			Verdict{},
		},
		{
			// A match id nobody offered.
			"matching against an unknown match", matching,
			Response{Pairs: map[uuid.UUID]uuid.UUID{optA: stranger, optB: matB}},
			Verdict{},
		},
		{"matching unanswered", matching, Response{}, Verdict{}},

		// Essays are nobody's job here, and a long one is not a right one.
		{"an essay is for a person", essay, Response{Text: "Because."}, Verdict{Manual: true}},
		{"an empty essay is still for a person", essay, Response{}, Verdict{Manual: true}},

		// A type this build cannot grade scores zero rather than passing silently.
		{"an unknown type", Question{Type: "graph", Points: 5}, Response{Text: "x"}, Verdict{}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := grade(test.question, test.response); got != test.want {
				t.Errorf("grade(%s) = %+v, want %+v", test.question.Type, got, test.want)
			}
		})
	}
}

// The zero value denies. A question no branch handles, a response nobody filled
// in: neither earns a point.
func TestTheZeroVerdictAwardsNothing(t *testing.T) {
	t.Parallel()

	var v Verdict
	if v.Correct || v.Points != 0 || v.Manual {
		t.Fatalf("the zero Verdict is %+v; it must award nothing and ask nobody", v)
	}
}

// Every type this package admits must be graded by a branch of grade(), or it
// would sit in an attempt forever, holding it out of `graded`.
func TestEveryValidTypeIsGraded(t *testing.T) {
	t.Parallel()

	types := []string{
		TypeTrueFalse, TypeSingleChoice, TypeMultipleChoice, TypeFillBlanks,
		TypeShortAnswer, TypeOrdering, TypeMatching, TypeOpenEnded,
	}

	for _, questionType := range types {
		if !ValidQuestionType(questionType) {
			t.Errorf("%s is a constant of this package but ValidQuestionType denies it", questionType)
		}

		// A correct answer to a well-formed question of each type must earn its
		// points, or be handed to a person. Anything else means grade() has no case
		// for it and fell through to the zero Verdict.
		question, response := wellFormed(questionType)

		got := grade(question, response)
		if IsManual(questionType) {
			if !got.Manual {
				t.Errorf("%s: grade() = %+v, want Manual", questionType, got)
			}
			continue
		}
		if !got.Correct || got.Points != question.Points {
			t.Errorf("%s: a correct answer graded %+v, want the question's %d points — "+
				"grade() has no case for this type", questionType, got, question.Points)
		}
	}
}

// wellFormed returns a question of the given type and an answer that is right.
func wellFormed(questionType string) (Question, Response) {
	q := Question{Type: questionType, Points: 7}

	switch questionType {
	case TypeTrueFalse, TypeSingleChoice:
		q.Options = []Option{choice(optA, true, 0), choice(optB, false, 1)}
		return q, Response{Choices: []uuid.UUID{optA}}

	case TypeMultipleChoice:
		q.Options = []Option{choice(optA, true, 0), choice(optB, false, 1)}
		return q, Response{Choices: []uuid.UUID{optA}}

	case TypeShortAnswer:
		q.Accepted = [][]string{{"yes"}}
		return q, Response{Text: "yes"}

	case TypeFillBlanks:
		q.Accepted = [][]string{{"yes"}, {"no"}}
		return q, Response{Blanks: []string{"yes", "no"}}

	case TypeOrdering:
		q.Options = []Option{choice(optA, false, 0), choice(optB, false, 1)}
		return q, Response{Order: []uuid.UUID{optA, optB}}

	case TypeMatching:
		q.Options = []Option{{ID: optA, MatchID: matA}, {ID: optB, MatchID: matB}}
		return q, Response{Pairs: map[uuid.UUID]uuid.UUID{optA: matA, optB: matB}}

	case TypeOpenEnded:
		return q, Response{Text: "An answer of some length."}
	}

	return q, Response{}
}

// A quiz is scored out of all its points, and one essay is enough to hold the
// whole attempt out of a final grade.
func TestGradeAttempt(t *testing.T) {
	t.Parallel()

	q1 := uuid.MustParse("00000000-0000-0000-0000-000000000011")
	q2 := uuid.MustParse("00000000-0000-0000-0000-000000000022")
	q3 := uuid.MustParse("00000000-0000-0000-0000-000000000033")

	questions := []Question{
		{ID: q1, Type: TypeTrueFalse, Points: 2, Options: []Option{choice(optA, true, 0), choice(optB, false, 1)}},
		{ID: q2, Type: TypeShortAnswer, Points: 3, Accepted: [][]string{{"paris"}}},
		{ID: q3, Type: TypeSingleChoice, Points: 5, Options: []Option{choice(optA, false, 0), choice(optB, true, 1)}},
	}

	t.Run("an unanswered question is graded, not skipped", func(t *testing.T) {
		t.Parallel()

		// Only the first is answered, and rightly. The other two still count against
		// the total, or leaving a question blank would be free.
		result := gradeAttempt(questions, map[uuid.UUID]Response{q1: {Choices: []uuid.UUID{optA}}})

		if result.Points != 2 || result.MaxPoints != 10 {
			t.Errorf("scored %d/%d, want 2/10", result.Points, result.MaxPoints)
		}
		if len(result.Answers) != 3 {
			t.Fatalf("graded %d answers, want one per question", len(result.Answers))
		}
		if result.AwaitingReview {
			t.Error("nothing here needs a person")
		}
	})

	t.Run("everything right", func(t *testing.T) {
		t.Parallel()

		result := gradeAttempt(questions, map[uuid.UUID]Response{
			q1: {Choices: []uuid.UUID{optA}},
			q2: {Text: "Paris"},
			q3: {Choices: []uuid.UUID{optB}},
		})

		if result.Points != 10 || result.MaxPoints != 10 {
			t.Errorf("scored %d/%d, want 10/10", result.Points, result.MaxPoints)
		}
	})

	t.Run("one essay holds the attempt out of a final grade", func(t *testing.T) {
		t.Parallel()

		withEssay := append(questions, Question{
			ID: uuid.MustParse("00000000-0000-0000-0000-000000000044"), Type: TypeOpenEnded, Points: 10,
		})

		result := gradeAttempt(withEssay, map[uuid.UUID]Response{q1: {Choices: []uuid.UUID{optA}}})

		if !result.AwaitingReview {
			t.Error("an essay is nobody's job here; the attempt must await review")
		}
		// The essay's points are available and not yet awarded.
		if result.Points != 2 || result.MaxPoints != 20 {
			t.Errorf("scored %d/%d, want 2/20", result.Points, result.MaxPoints)
		}
	})

	t.Run("a response to a question that is not on the quiz is ignored", func(t *testing.T) {
		t.Parallel()

		result := gradeAttempt(questions, map[uuid.UUID]Response{
			q1:       {Choices: []uuid.UUID{optA}},
			stranger: {Text: "paris"},
		})

		if result.Points != 2 || len(result.Answers) != 3 {
			t.Errorf("scored %d with %d answers, want 2 with 3", result.Points, len(result.Answers))
		}
	})

	t.Run("an empty quiz", func(t *testing.T) {
		t.Parallel()

		result := gradeAttempt(nil, nil)
		if result.Points != 0 || result.MaxPoints != 0 || result.AwaitingReview {
			t.Errorf("%+v, want an empty result", result)
		}
	})
}

// The bar is compared in integers, so a score exactly on it passes.
func TestPassed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                          string
		points, maxPoints, passingPct int
		want                          bool
	}{
		{"exactly on the bar", 3, 5, 60, true},
		{"one point under", 2, 5, 60, false},
		{"over", 4, 5, 60, true},
		{"a bar of zero passes everyone", 0, 5, 0, true},
		{"a bar of a hundred needs every point", 4, 5, 100, false},
		{"a hundred percent, exactly", 5, 5, 100, true},

		// Scoring out of nothing. Refusing here would fail a learner for the author's
		// empty quiz; passing says the only true thing: they scored all of it.
		{"a quiz worth nothing", 0, 0, 80, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := passed(test.points, test.maxPoints, test.passingPct); got != test.want {
				t.Errorf("passed(%d, %d, %d) = %v, want %v",
					test.points, test.maxPoints, test.passingPct, got, test.want)
			}
		})
	}
}
