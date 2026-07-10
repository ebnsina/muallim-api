package main

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/grade"
)

/*
The adapter that lets `assess` write to the gradebook.

The worker grades quiz attempts. It does not mark assignments — a person does
that, through the API — so only one of the two adapters is here.

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
