// Package exams models institution assessments: grading scales, exams, marks, and
// the report cards computed from them. It knows nothing about HTTP, and references
// the academic spine (terms, classes, students, subjects) by id, never by import.
package exams

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Sentinels. internal/httpapi maps each to a status; add a line to errors_test.go
// in the same commit as a new one.
var (
	ErrNotFound      = errors.New("exams: not found")
	ErrInvalidScale  = errors.New("exams: the grading scale is not valid")
	ErrInvalidExam   = errors.New("exams: the exam is not valid")
	ErrInvalidMark   = errors.New("exams: the mark is not valid")
	ErrNoScale       = errors.New("exams: this workspace has no grading scale")
	ErrUngraded      = errors.New("exams: a mark fell outside every band of its scale")
	ErrInvalidPage   = errors.New("exams: the page cursor is not valid")
	ErrExamPublished = errors.New("exams: a published exam cannot be edited")
)

// Exam status.
const (
	StatusDraft     = "draft"
	StatusPublished = "published"
)

// Bounds. An institution has a handful of scales and a bounded exam calendar.
const (
	MaxScales    = 50
	MaxBands     = 30
	MaxMarks     = 2000
	MaxExamsPage = 100
)

// Audit actions.
const (
	ActionScaleCreated  = "grading_scale.created"
	ActionExamCreated   = "exam.created"
	ActionExamPublished = "exam.published"
	ActionMarksEntered  = "exam.marks_entered"
)

// DefaultScaleName is the seeded Bangladesh board scale.
const DefaultScaleName = "GPA 5.0 (Bangladesh)"

// bdGPA5 is the public secondary board's 5.0 scale: A+ at 80, down to F below 33.
// A score lands in the highest band whose floor it clears.
var bdGPA5 = []Band{
	{Letter: "A+", MinPercent: 80, GPAPoint: 5.0, IsPass: true},
	{Letter: "A", MinPercent: 70, GPAPoint: 4.0, IsPass: true},
	{Letter: "A-", MinPercent: 60, GPAPoint: 3.5, IsPass: true},
	{Letter: "B", MinPercent: 50, GPAPoint: 3.0, IsPass: true},
	{Letter: "C", MinPercent: 40, GPAPoint: 2.0, IsPass: true},
	{Letter: "D", MinPercent: 33, GPAPoint: 1.0, IsPass: true},
	{Letter: "F", MinPercent: 0, GPAPoint: 0.0, IsPass: false},
}

// Band is one row of a grading scale.
type Band struct {
	Letter     string
	MinPercent float64
	GPAPoint   float64
	IsPass     bool
}

// GradingScale is a named table of bands.
type GradingScale struct {
	ID        uuid.UUID
	Name      string
	IsDefault bool
	Bands     []Band
	CreatedAt time.Time
}

// NewScale is a scale to create.
type NewScale struct {
	Name      string
	IsDefault bool
	Bands     []Band
}

// Exam is a named assessment event, graded against one scale.
type Exam struct {
	ID           uuid.UUID
	Name         string
	TermID       *uuid.UUID
	GradeLevelID *uuid.UUID
	ScaleID      uuid.UUID
	HeldOn       *time.Time
	Status       string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// NewExam is an exam to create.
type NewExam struct {
	Name         string
	TermID       *uuid.UUID
	GradeLevelID *uuid.UUID
	ScaleID      uuid.UUID
	HeldOn       *time.Time
}

// Mark is one subject's score for one student in one exam.
type Mark struct {
	StudentID uuid.UUID
	SubjectID uuid.UUID
	FullMarks float64
	Obtained  float64
}

// markRow is a stored mark read back with its subject's name.
type markRow struct {
	StudentID   uuid.UUID
	SubjectID   uuid.UUID
	SubjectName string
	FullMarks   float64
	Obtained    float64
}

// SubjectResult is one subject's graded line on a report card.
type SubjectResult struct {
	SubjectID   uuid.UUID
	SubjectName string
	FullMarks   float64
	Obtained    float64
	Percent     float64
	Letter      string
	GPAPoint    float64
	IsPass      bool
}

// ReportCard is a student's whole result for an exam, computed from their marks.
type ReportCard struct {
	StudentID      uuid.UUID
	Subjects       []SubjectResult
	TotalObtained  float64
	TotalFull      float64
	AveragePercent float64
	GPA            float64
	OverallLetter  string
	Passed         bool
}

func (n NewScale) validate() error {
	if n.Name == "" {
		return fmt.Errorf("%w: name it", ErrInvalidScale)
	}
	if len(n.Bands) == 0 {
		return fmt.Errorf("%w: give it at least one band", ErrInvalidScale)
	}
	if len(n.Bands) > MaxBands {
		return fmt.Errorf("%w: at most %d bands", ErrInvalidScale, MaxBands)
	}
	for _, b := range n.Bands {
		if b.Letter == "" {
			return fmt.Errorf("%w: every band needs a letter", ErrInvalidScale)
		}
		if b.MinPercent < 0 || b.MinPercent > 100 {
			return fmt.Errorf("%w: a floor is a percentage, 0 to 100", ErrInvalidScale)
		}
	}
	return nil
}

func (n NewExam) validate() error {
	if n.Name == "" {
		return fmt.Errorf("%w: name it", ErrInvalidExam)
	}
	if n.ScaleID == uuid.Nil {
		return fmt.Errorf("%w: choose a grading scale", ErrInvalidExam)
	}
	return nil
}

func (m Mark) validate() error {
	if m.FullMarks <= 0 {
		return fmt.Errorf("%w: full marks must be positive", ErrInvalidMark)
	}
	if m.Obtained < 0 || m.Obtained > m.FullMarks {
		return fmt.Errorf("%w: obtained must be between 0 and the full marks", ErrInvalidMark)
	}
	return nil
}

// grade finds the band a percentage falls into: the highest band whose floor it
// clears. bands must be sorted by MinPercent descending. It is the one place a
// score becomes a letter, so a report card cannot grade a mark two ways.
func grade(percent float64, bands []Band) (Band, bool) {
	for _, b := range bands {
		if percent >= b.MinPercent {
			return b, true
		}
	}
	return Band{}, false
}

// reportCard computes a student's result from their marks against a scale. Overall
// GPA is the mean of the subjects' grade points; a single failing subject fails the
// exam, which is how the board reads it.
func reportCard(studentID uuid.UUID, marks []markRow, bands []Band) (ReportCard, error) {
	rc := ReportCard{StudentID: studentID, Passed: len(marks) > 0}
	var gpaSum float64
	for _, m := range marks {
		percent := m.Obtained / m.FullMarks * 100
		band, ok := grade(percent, bands)
		if !ok {
			return ReportCard{}, fmt.Errorf("%w: %.2f%% in %s", ErrUngraded, percent, m.SubjectName)
		}
		rc.Subjects = append(rc.Subjects, SubjectResult{
			SubjectID: m.SubjectID, SubjectName: m.SubjectName,
			FullMarks: m.FullMarks, Obtained: m.Obtained, Percent: percent,
			Letter: band.Letter, GPAPoint: band.GPAPoint, IsPass: band.IsPass,
		})
		rc.TotalObtained += m.Obtained
		rc.TotalFull += m.FullMarks
		gpaSum += band.GPAPoint
		if !band.IsPass {
			rc.Passed = false
		}
	}
	if n := len(marks); n > 0 {
		rc.GPA = gpaSum / float64(n)
		rc.AveragePercent = rc.TotalObtained / rc.TotalFull * 100
		if band, ok := grade(rc.AveragePercent, bands); ok {
			rc.OverallLetter = band.Letter
		}
	}
	return rc, nil
}
