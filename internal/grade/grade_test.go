package grade

import (
	"testing"

	"github.com/google/uuid"
)

/*
Every percentage from 0 to 100 lands in exactly one band of the default scale.

Not a spot check of the boundaries. A scale is a partition of the whole range, and
the way a partition breaks is a gap or an overlap somewhere nobody sampled.
*/
func TestTheDefaultScaleCoversEveryPercentage(t *testing.T) {
	t.Parallel()

	scale := DefaultScale()

	for percent := 0; percent <= 100; percent++ {
		band := scale.Band(percent)

		if band.Label == "" {
			t.Fatalf("%d%% falls through the scale", percent)
		}
		if percent < band.Min {
			t.Errorf("%d%% was graded %q, whose floor is %d%%", percent, band.Label, band.Min)
		}
	}
}

// The floors, exactly. 90 is an A and 89 is a B; a scale that got this wrong
// would be wrong by one mark for every learner on the boundary.
func TestTheDefaultScaleAtItsBoundaries(t *testing.T) {
	t.Parallel()

	scale := DefaultScale()

	for percent, want := range map[int]string{
		100: "A", 90: "A", 89: "B",
		80: "B", 79: "C",
		70: "C", 69: "D",
		60: "D", 59: "F",
		0: "F",
	} {
		if got := scale.Band(percent).Label; got != want {
			t.Errorf("%d%% graded %q, want %q", percent, got, want)
		}
	}
}

// A pass is a property of the band, not a percentage this package knows. The
// author of the scale decides where the line is.
func TestTheDefaultScaleFailsOnlyF(t *testing.T) {
	t.Parallel()

	scale := DefaultScale()

	if scale.Band(59).IsPass {
		t.Error("59% passed")
	}
	if !scale.Band(60).IsPass {
		t.Error("60% did not pass")
	}
}

// The bands may arrive in any order — from a test, or from a query somebody
// changed the ORDER BY on. The answer must not depend on it.
func TestBandsAreSearchedFromTheTopWhateverOrderTheyArriveIn(t *testing.T) {
	t.Parallel()

	shuffled := Scale{Bands: []Band{
		{Label: "F", Min: 0},
		{Label: "A", Min: 90, IsPass: true},
		{Label: "C", Min: 70, IsPass: true},
		{Label: "B", Min: 80, IsPass: true},
	}}

	if got := shuffled.Band(85).Label; got != "B" {
		t.Errorf("85%% graded %q, want B", got)
	}
	if got := shuffled.Band(95).Label; got != "A" {
		t.Errorf("95%% graded %q, want A", got)
	}
}

func TestScaleValidation(t *testing.T) {
	t.Parallel()

	if err := DefaultScale().validate(); err != nil {
		t.Errorf("the default scale is invalid: %v", err)
	}

	good := Scale{Name: "Pass/fail", Bands: []Band{
		{Label: "Pass", Min: 50, IsPass: true},
		{Label: "Fail", Min: 0},
	}}
	if err := good.validate(); err != nil {
		t.Errorf("a two-band scale was refused: %v", err)
	}

	bad := map[string]Scale{
		"no name":  {Bands: good.Bands},
		"no bands": {Name: "Empty"},

		// A learner who scored 3% would have no grade at all.
		"nothing covers zero": {Name: "Gap", Bands: []Band{
			{Label: "Pass", Min: 50, IsPass: true},
			{Label: "Fail", Min: 10},
		}},

		// The letter would depend on which row the planner reached first.
		"two bands at one floor": {Name: "Ambiguous", Bands: []Band{
			{Label: "Pass", Min: 50, IsPass: true},
			{Label: "Merit", Min: 50, IsPass: true},
			{Label: "Fail", Min: 0},
		}},

		// A scale nobody can pass is a scale somebody mistyped.
		"nothing is a pass": {Name: "Cruel", Bands: []Band{
			{Label: "Fail", Min: 0},
			{Label: "Also fail", Min: 50},
		}},

		"a band above 100": {Name: "Impossible", Bands: []Band{
			{Label: "Perfect", Min: 101, IsPass: true},
			{Label: "Fail", Min: 0},
		}},
		"a band below 0": {Name: "Negative", Bands: []Band{
			{Label: "Pass", Min: -1, IsPass: true},
			{Label: "Fail", Min: 0},
		}},
		"a band with no label": {Name: "Silent", Bands: []Band{
			{Label: " ", Min: 50, IsPass: true},
			{Label: "Fail", Min: 0},
		}},
	}

	for name, scale := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := scale.validate(); err == nil {
				t.Errorf("%s was accepted", name)
			}
		})
	}
}

func item(max int) Item { return Item{ID: uuid.New(), MaxPoints: max} }

func entry(i Item, points int) Entry {
	return Entry{ItemID: i.ID, Points: points, MaxPoints: i.MaxPoints}
}

/*
A course grade counts the work that has been marked, and nothing else.

Scoring the unattempted items zero tells a learner who has done the first of ten
assessments perfectly that they are failing at 10%, on the day they are most
likely to give up. `Graded` of `ItemCount` is how the page says what the number
covers.
*/
func TestOnlyGradedItemsCount(t *testing.T) {
	t.Parallel()

	first, second, third := item(10), item(10), item(10)
	result := Summarise(
		[]Item{first, second, third},
		[]Entry{entry(first, 10)},
		DefaultScale(),
	)

	if result.Percent != 100 {
		t.Errorf("percent = %d, want 100", result.Percent)
	}
	if result.Band.Label != "A" {
		t.Errorf("band = %q, want A", result.Band.Label)
	}
	if result.Graded != 1 || result.ItemCount != 3 {
		t.Errorf("graded %d of %d, want 1 of 3", result.Graded, result.ItemCount)
	}
}

// Nothing marked is not a zero. It is an absence, and an F for work nobody has
// been asked to hand in yet is a lie a learner will believe.
func TestAnUngradedLearnerHasNoBand(t *testing.T) {
	t.Parallel()

	result := Summarise([]Item{item(10), item(10)}, nil, DefaultScale())

	if result.Graded != 0 {
		t.Errorf("graded = %d, want 0", result.Graded)
	}
	if result.Percent != 0 {
		t.Errorf("percent = %d, want 0", result.Percent)
	}
	if result.Band.Label != "" {
		t.Errorf("band = %q, want none", result.Band.Label)
	}
}

/*
The item's worth is taken from the entry, not from the item as it stands now.

An author who raises a quiz from 10 points to 20 has not retroactively halved
everybody's grade. The entry records what the thing was worth when it was marked.
*/
func TestAnEntryRemembersWhatTheItemWasWorth(t *testing.T) {
	t.Parallel()

	quiz := item(10)
	marked := entry(quiz, 8) // 8 of 10, at the time.

	quiz.MaxPoints = 20 // The author doubles it afterwards.

	result := Summarise([]Item{quiz}, []Entry{marked}, DefaultScale())

	if result.MaxPoints != 10 {
		t.Errorf("max points = %d, want the 10 it was marked out of", result.MaxPoints)
	}
	if result.Percent != 80 {
		t.Errorf("percent = %d, want 80", result.Percent)
	}
}

/*
Rounded half up, on the total, once.

Rounding each item and averaging the roundings is a different number, and it is
the wrong one: three items at 2 of 3 are 66.67%, not 67%.
*/
func TestPercentRoundsOnceOnTheTotal(t *testing.T) {
	t.Parallel()

	one, two, three := item(3), item(3), item(3)
	result := Summarise(
		[]Item{one, two, three},
		[]Entry{entry(one, 2), entry(two, 2), entry(three, 2)},
		DefaultScale(),
	)

	// 6 of 9 is 66.67%, which rounds to 67.
	if result.Percent != 67 {
		t.Errorf("percent = %d, want 67", result.Percent)
	}

	// And the half-up boundary: 1 of 8 is 12.5%.
	eighth := item(8)
	half := Summarise([]Item{eighth}, []Entry{entry(eighth, 1)}, DefaultScale())
	if half.Percent != 13 {
		t.Errorf("12.5%% rounded to %d, want 13", half.Percent)
	}

	// 3 of 8 is 37.5%, which also rounds up. Half-up, not half-to-even.
	three8 := item(8)
	up := Summarise([]Item{three8}, []Entry{entry(three8, 3)}, DefaultScale())
	if up.Percent != 38 {
		t.Errorf("37.5%% rounded to %d, want 38", up.Percent)
	}
}

// An entry for an item that is not in the course is not counted. It happens: a
// lesson is moved to another course, and the item goes with it.
func TestAnEntryForSomethingElseIsIgnored(t *testing.T) {
	t.Parallel()

	mine, elsewhere := item(10), item(10)
	result := Summarise([]Item{mine}, []Entry{entry(mine, 5), entry(elsewhere, 0)}, DefaultScale())

	if result.Graded != 1 || result.MaxPoints != 10 {
		t.Errorf("graded %d items worth %d, want 1 worth 10", result.Graded, result.MaxPoints)
	}
}

func TestScoreValidation(t *testing.T) {
	t.Parallel()

	good := Score{Source: SourceQuiz, Title: "Chapter one", Points: 3, MaxPoints: 5}
	if err := good.validate(); err != nil {
		t.Errorf("a well-formed score was refused: %v", err)
	}

	bad := map[string]Score{
		"an unknown source":     {Source: "vibes", Title: "T", Points: 1, MaxPoints: 5},
		"worth nothing":         {Source: SourceQuiz, Title: "T", Points: 0, MaxPoints: 0},
		"more than the maximum": {Source: SourceQuiz, Title: "T", Points: 6, MaxPoints: 5},
		"negative points":       {Source: SourceQuiz, Title: "T", Points: -1, MaxPoints: 5},
		"no title":              {Source: SourceQuiz, Title: "  ", Points: 1, MaxPoints: 5},
	}

	for name, score := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := score.validate(); err == nil {
				t.Errorf("%s was accepted", name)
			}
		})
	}

	// Zero of five is a real grade, and a common one.
	if err := (Score{Source: SourceAssignment, Title: "Essay", Points: 0, MaxPoints: 5}).validate(); err != nil {
		t.Errorf("scoring zero was refused: %v", err)
	}
}
