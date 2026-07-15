package exams

import (
	"errors"
	"math"
	"testing"

	"github.com/google/uuid"
)

func TestGradeAgainstBDBands(t *testing.T) {
	t.Parallel()

	cases := []struct {
		percent float64
		letter  string
		gpa     float64
	}{
		{100, "A+", 5.0},
		{80, "A+", 5.0},
		{79.9, "A", 4.0},
		{70, "A", 4.0},
		{60, "A-", 3.5},
		{50, "B", 3.0},
		{40, "C", 2.0},
		{33, "D", 1.0},
		{32.9, "F", 0.0},
		{0, "F", 0.0},
	}
	for _, c := range cases {
		band, ok := grade(c.percent, bdGPA5)
		if !ok {
			t.Errorf("%.1f%% fell outside every band", c.percent)
			continue
		}
		if band.Letter != c.letter || band.GPAPoint != c.gpa {
			t.Errorf("%.1f%% graded %s/%.1f, want %s/%.1f", c.percent, band.Letter, band.GPAPoint, c.letter, c.gpa)
		}
	}
}

func TestReportCardComputesGPAAndPass(t *testing.T) {
	t.Parallel()

	student := uuid.New()
	marks := []markRow{
		{SubjectID: uuid.New(), SubjectName: "Bangla", FullMarks: 100, Obtained: 85},  // A+ / 5.0
		{SubjectID: uuid.New(), SubjectName: "English", FullMarks: 100, Obtained: 72}, // A / 4.0
		{SubjectID: uuid.New(), SubjectName: "Math", FullMarks: 100, Obtained: 55},    // B / 3.0
	}
	rc, err := reportCard(student, marks, bdGPA5)
	if err != nil {
		t.Fatalf("report card: %v", err)
	}
	if !rc.Passed {
		t.Error("a card with no failing subject should pass")
	}
	if wantGPA := (5.0 + 4.0 + 3.0) / 3; math.Abs(rc.GPA-wantGPA) > 1e-9 {
		t.Errorf("GPA %.4f, want %.4f", rc.GPA, wantGPA)
	}
	if rc.TotalObtained != 212 || rc.TotalFull != 300 {
		t.Errorf("totals %.0f/%.0f, want 212/300", rc.TotalObtained, rc.TotalFull)
	}
	if want := 212.0 / 300 * 100; math.Abs(rc.AveragePercent-want) > 1e-9 {
		t.Errorf("average %.4f%%, want %.4f%%", rc.AveragePercent, want)
	}
}

func TestReportCardFailsOnASingleF(t *testing.T) {
	t.Parallel()

	marks := []markRow{
		{SubjectID: uuid.New(), SubjectName: "Bangla", FullMarks: 100, Obtained: 90}, // A+
		{SubjectID: uuid.New(), SubjectName: "Math", FullMarks: 100, Obtained: 20},   // F — one is enough
	}
	rc, err := reportCard(uuid.New(), marks, bdGPA5)
	if err != nil {
		t.Fatalf("report card: %v", err)
	}
	if rc.Passed {
		t.Error("a single F must fail the whole exam, as the board reads it")
	}
}

func TestReportCardRejectsAnUngradableMark(t *testing.T) {
	t.Parallel()

	// A scale that does not reach zero: a very low score falls through every band.
	partial := []Band{{Letter: "A", MinPercent: 50, GPAPoint: 4, IsPass: true}}
	_, err := reportCard(uuid.New(), []markRow{
		{SubjectID: uuid.New(), SubjectName: "Math", FullMarks: 100, Obtained: 10},
	}, partial)
	if !errors.Is(err, ErrUngraded) {
		t.Fatalf("a mark below every band should be ErrUngraded, got %v", err)
	}
}
