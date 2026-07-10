package main

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/grade"
)

/*
The two adapters that let `assess` and `assign` write to the gradebook.

Neither imports `grade`, and `grade` imports neither. Each declares the method it
needs, in its own words, and this file — which is allowed to know about all three
— is where the wiring happens. It is the same seam `enroll` is reached through.

Both take the caller's transaction. The mark and the grade commit together, or
neither does.
*/

// quizGrades adapts the gradebook to `assess.Grades`.
type quizGrades struct{ svc *grade.Service }

func (g quizGrades) RecordScore(ctx context.Context, tx pgx.Tx, tenantID, lessonID, userID, sourceID uuid.UUID,
	title string, points, maxPoints int, keepHighest bool,
) error {
	return g.svc.Record(ctx, tx, tenantID, grade.Score{
		LessonID: lessonID, UserID: userID,
		Source: grade.SourceQuiz, SourceID: sourceID,
		Title: title, Points: points, MaxPoints: maxPoints,
		KeepHighest: keepHighest,
	})
}

// assignmentGrades adapts the gradebook to `assign.Grades`.
//
// No `keepHighest`. An assignment is marked rather than attempted, and a marker
// who lowers a grade means to lower it.
type assignmentGrades struct{ svc *grade.Service }

func (g assignmentGrades) RecordMark(ctx context.Context, tx pgx.Tx, tenantID, lessonID, userID, sourceID uuid.UUID,
	title string, points, maxPoints int,
) error {
	return g.svc.Record(ctx, tx, tenantID, grade.Score{
		LessonID: lessonID, UserID: userID,
		Source: grade.SourceAssignment, SourceID: sourceID,
		Title: title, Points: points, MaxPoints: maxPoints,
	})
}

func (g quizGrades) EnsureItem(ctx context.Context, tx pgx.Tx, tenantID, lessonID, sourceID uuid.UUID,
	title string, maxPoints int,
) error {
	return g.svc.EnsureItem(ctx, tx, tenantID, grade.Score{
		LessonID: lessonID,
		Source:   grade.SourceQuiz, SourceID: sourceID,
		Title: title, MaxPoints: maxPoints,
	})
}

func (g assignmentGrades) EnsureItem(ctx context.Context, tx pgx.Tx, tenantID, lessonID, sourceID uuid.UUID,
	title string, maxPoints int,
) error {
	return g.svc.EnsureItem(ctx, tx, tenantID, grade.Score{
		LessonID: lessonID,
		Source:   grade.SourceAssignment, SourceID: sourceID,
		Title: title, MaxPoints: maxPoints,
	})
}
