package assess

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

// Response is a learner's answer, in the one shape that covers every type.
//
// Exactly one field is meaningful per question type, and grade() reads only that
// one. A response carrying the wrong field is not an error: it is a wrong answer,
// which is what an empty response is too. Nothing here can be made to panic by a
// request body.
type Response struct {
	// Choices are the option ids the learner picked: one for true_false and
	// single_choice, any number for multiple_choice.
	Choices []uuid.UUID `json:"choices,omitempty"`

	// Text is a typed answer: short_answer, open_ended.
	Text string `json:"text,omitempty"`

	// Blanks are the typed fills, in the order the blanks appear in the prompt.
	Blanks []string `json:"blanks,omitempty"`

	// Order is the option ids in the sequence the learner arranged them.
	Order []uuid.UUID `json:"order,omitempty"`

	// Pairs maps each option id to the id of the match the learner put beside it.
	Pairs map[uuid.UUID]uuid.UUID `json:"pairs,omitempty"`

	// Number is a numeric answer: range. A pointer so that "not answered" and "the
	// number zero" are different — zero is a legitimate answer to grade.
	Number *float64 `json:"number,omitempty"`

	// Point is where the learner placed a marker: pin. A pointer so an unplaced pin
	// is distinct from one placed at the origin.
	Point *Point `json:"point,omitempty"`

	// Points are the coordinates the learner plotted: graph.
	Points []Point `json:"points,omitempty"`

	// Upload is the object-store key of an uploaded answer image: draw_image. The
	// bytes never transit the API; the learner PUTs them to a presigned URL and
	// sends the key here.
	Upload string `json:"upload,omitempty"`
}

// Verdict is what grading concluded about one answer.
//
// The zero value awards nothing and asks nobody: an unanswered question, or a
// question of a type this build does not grade, scores zero rather than passing
// silently. Denying by default is the only safe direction for a grade.
type Verdict struct {
	// Manual means no machine graded this, and an instructor must. Points is zero
	// and Correct is false until they do.
	Manual bool

	Correct bool
	Points  int
}

// grade judges one answer. It is pure: no clock, no database, no randomness.
//
// Credit is all-or-nothing. A question worth three points is worth three points
// when it is right and none when it is not, and there is no partial credit for
// naming two of three correct options — an author who wants finer resolution
// splits the question. This is a rule, not a rounding accident, and it is the
// reason points can stay integers.
func grade(q Question, r Response) Verdict {
	if IsManual(q.Type) {
		return Verdict{Manual: true}
	}

	var correct bool
	switch q.Type {
	case TypeTrueFalse, TypeSingleChoice, TypeImageAnswering:
		correct = gradeSingleChoice(q, r)
	case TypeMultipleChoice:
		correct = gradeMultipleChoice(q, r)
	case TypeShortAnswer:
		correct = gradeBlanks(q, []string{r.Text})
	case TypeFillBlanks:
		correct = gradeBlanks(q, r.Blanks)
	case TypeOrdering, TypePuzzle:
		correct = gradeOrdering(q, r)
	case TypeMatching, TypeImageMatching:
		correct = gradeMatching(q, r)
	case TypeRange:
		correct = gradeRange(q, r)
	case TypePin:
		correct = gradePin(q, r)
	case TypeGraph:
		correct = gradeGraph(q, r)
	default:
		// A type nothing here grades. It cannot reach the database — authoring
		// refuses it — so arriving is a bug, and the safe answer to a bug is zero.
		return Verdict{}
	}

	if !correct {
		return Verdict{}
	}
	return Verdict{Correct: true, Points: q.Points}
}

// AnswerGrade is one question's verdict, ready to be written back.
type AnswerGrade struct {
	QuestionID uuid.UUID
	Verdict    Verdict
}

// Result is what grading an attempt concluded.
type Result struct {
	Answers []AnswerGrade

	// Points is what the machine awarded. An attempt still awaiting review will
	// gain more.
	Points int

	// MaxPoints is the sum of every question's points, whether or not the learner
	// answered it. A quiz is scored out of itself, not out of what was attempted.
	MaxPoints int

	// AwaitingReview means at least one question needs a person.
	AwaitingReview bool
}

// gradeAttempt judges every question of the quiz, including the ones the learner
// left alone.
//
// Unanswered is graded, not skipped: a question with no response earns the zero
// Verdict, and a quiz is scored out of all its points. Scoring out of the
// attempted points would make abandoning a question the best move whenever you
// did not know the answer.
//
// Pure. The job that calls this does the reading and the writing; the arithmetic
// happens here, where a table test can reach it.
func gradeAttempt(questions []Question, responses map[uuid.UUID]Response) Result {
	result := Result{Answers: make([]AnswerGrade, 0, len(questions))}

	for _, q := range questions {
		verdict := grade(q, responses[q.ID])

		result.MaxPoints += q.Points
		result.Points += verdict.Points
		if verdict.Manual {
			result.AwaitingReview = true
		}

		result.Answers = append(result.Answers, AnswerGrade{QuestionID: q.ID, Verdict: verdict})
	}

	return result
}

// passed reports whether points clear the quiz's bar.
//
// The comparison is `points * 100 >= passing * max`, in integers, so a quiz with
// a 60% bar and a score of 3 out of 5 passes exactly — no float is asked whether
// 0.6 is at least 0.6. A quiz worth nothing is passed by anyone, which is the
// only reading of "you scored all of the available points" that is not a lie.
func passed(points, maxPoints, passingPercent int) bool {
	if maxPoints <= 0 {
		return true
	}
	return points*100 >= passingPercent*maxPoints
}

// gradeSingleChoice wants exactly one choice, and it must be the correct one.
//
// A question with no correct option, or with two, is one an author should not
// have been able to save; here it simply cannot be answered right.
func gradeSingleChoice(q Question, r Response) bool {
	if len(r.Choices) != 1 {
		return false
	}

	var correctID uuid.UUID
	var found int
	for _, o := range q.Options {
		if o.IsCorrect {
			correctID = o.ID
			found++
		}
	}
	if found != 1 {
		return false
	}

	return r.Choices[0] == correctID
}

// gradeMultipleChoice wants the correct set exactly: every right option, and no
// wrong one.
//
// Duplicates in the response are collapsed rather than counted. A client that
// sends the same id twice has said one thing twice, not two things.
func gradeMultipleChoice(q Question, r Response) bool {
	want := make(map[uuid.UUID]struct{})
	for _, o := range q.Options {
		if o.IsCorrect {
			want[o.ID] = struct{}{}
		}
	}
	if len(want) == 0 {
		return false
	}

	got := make(map[uuid.UUID]struct{}, len(r.Choices))
	for _, id := range r.Choices {
		got[id] = struct{}{}
	}
	if len(got) != len(want) {
		return false
	}

	for id := range want {
		if _, ok := got[id]; !ok {
			return false
		}
	}
	return true
}

// gradeBlanks compares each typed answer against the spellings its blank accepts.
//
// Every blank must be filled: an answer that omits one is not partly right, it is
// wrong, because credit is all-or-nothing.
// gradeRange is right when the answer is a number within the author's bounds,
// stored as the single pair Accepted[0] = [min, max].
func gradeRange(q Question, r Response) bool {
	if r.Number == nil || len(q.Accepted) != 1 || len(q.Accepted[0]) != 2 {
		return false
	}
	lo, err1 := strconv.ParseFloat(strings.TrimSpace(q.Accepted[0][0]), 64)
	hi, err2 := strconv.ParseFloat(strings.TrimSpace(q.Accepted[0][1]), 64)
	if err1 != nil || err2 != nil {
		return false
	}
	return *r.Number >= lo && *r.Number <= hi
}

func gradeBlanks(q Question, typed []string) bool {
	if len(q.Accepted) == 0 || len(typed) != len(q.Accepted) {
		return false
	}

	for i, accepted := range q.Accepted {
		given := normalise(typed[i], q.CaseSensitive)
		if given == "" {
			return false
		}

		var matched bool
		for _, want := range accepted {
			if given == normalise(want, q.CaseSensitive) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// gradeOrdering compares the learner's sequence with the author's positions.
//
// The learner must place every option, once. A short sequence is wrong rather
// than partly right, and a repeated id cannot make a sequence correct because the
// lengths must match and the author's own order contains no repeats.
func gradeOrdering(q Question, r Response) bool {
	if len(q.Options) == 0 || len(r.Order) != len(q.Options) {
		return false
	}

	want := slices.Clone(q.Options)
	slices.SortFunc(want, func(a, b Option) int { return a.Position - b.Position })

	for i, o := range want {
		if r.Order[i] != o.ID {
			return false
		}
	}
	return true
}

// gradeMatching wants every option paired with its own match, and nothing else
// paired at all.
//
// The learner's map is keyed by option id and valued by match id. Those are
// separate identifiers precisely so that receiving the list of matches tells you
// nothing about which option each belongs to.
func gradeMatching(q Question, r Response) bool {
	if len(q.Options) == 0 || len(r.Pairs) != len(q.Options) {
		return false
	}

	for _, o := range q.Options {
		if r.Pairs[o.ID] != o.MatchID {
			return false
		}
	}
	return true
}

// gradePin is right when the learner's point lands inside any hotspot region the
// author drew. No point placed, or a question with no regions, is wrong — there is
// nothing to be right about.
func gradePin(q Question, r Response) bool {
	if r.Point == nil || q.Spec == nil || len(q.Spec.Regions) == 0 {
		return false
	}
	for _, reg := range q.Spec.Regions {
		if r.Point.X >= reg.X && r.Point.X <= reg.X+reg.W &&
			r.Point.Y >= reg.Y && r.Point.Y <= reg.Y+reg.H {
			return true
		}
	}
	return false
}

// gradeGraph is right when every expected point is matched, once, by one of the
// learner's within the tolerance — and no extra point is plotted, because credit
// is all-or-nothing and a superfluous point is a different answer. Each learner
// point is spent on at most one expectation, so plotting the same point twice
// cannot cover two expectations.
func gradeGraph(q Question, r Response) bool {
	if q.Spec == nil || len(q.Spec.Points) == 0 || len(r.Points) != len(q.Spec.Points) {
		return false
	}
	tol := q.Spec.Tolerance
	spent := make([]bool, len(r.Points))
	for _, want := range q.Spec.Points {
		matched := false
		for i, got := range r.Points {
			if spent[i] {
				continue
			}
			if math.Abs(got.X-want.X) <= tol && math.Abs(got.Y-want.Y) <= tol {
				spent[i] = true
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// normalise makes two typed answers comparable: leading and trailing space gone,
// runs of internal whitespace collapsed to one, and case folded unless the author
// asked for it to matter.
//
// A learner who typed two spaces between words has not answered a different
// question.
func normalise(s string, caseSensitive bool) string {
	s = strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
	if caseSensitive {
		return s
	}
	return strings.ToLower(s)
}
