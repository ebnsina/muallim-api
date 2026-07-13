package enroll

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgresRepository satisfies Repository.
//
// It reads the courses, topics, and lessons tables directly rather than calling
// the catalog package: a domain package may not import a sibling, and the
// alternative — an interface catalog implements — would buy nothing here except
// a round trip per lesson. The tables are a shared schema, not catalog's private
// property.
type PostgresRepository struct{}

// NewPostgresRepository returns a Repository backed by Postgres.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

// courseForEnrolmentSQL finds a course a learner may enrol in.
//
// Only a published course is enrollable. An unpublished one is not "closed", it
// is invisible: telling a stranger that a draft exists is exactly the leak the
// catalog read path was fixed for.
const courseForEnrolmentSQL = `
	SELECT id, status FROM courses
	WHERE tenant_id = $1 AND lower(slug) = lower($2)`

// CourseBySlug returns a course's id and status, whatever the status.
func (r *PostgresRepository) CourseBySlug(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string) (uuid.UUID, string, error) {
	var (
		id     uuid.UUID
		status string
	)
	err := tx.QueryRow(ctx, courseForEnrolmentSQL, tenantID, slug).Scan(&id, &status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, "", ErrNotFound
		}
		return uuid.Nil, "", fmt.Errorf("enroll: load course %q: %w", slug, err)
	}
	return id, status, nil
}

// upsertEnrolmentSQL enrols, or reactivates a lapsed enrolment.
//
// Re-enrolling must not create a second row: progress hangs off (user, course),
// and a learner who cancels and returns expects to find their place. ON CONFLICT
// reactivates instead, and the unique index makes that atomic under a race.
const upsertEnrolmentSQL = `
	INSERT INTO enrolments (tenant_id, course_id, user_id, source, expires_at)
	VALUES ($1, $2, $3, $4, $5)
	ON CONFLICT (tenant_id, course_id, user_id) DO UPDATE
	SET status     = CASE WHEN enrolments.status IN ('expired', 'cancelled')
	                      THEN 'active' ELSE enrolments.status END,
	    -- Reactivating a lapsed enrolment must not leave the timestamp of a
	    -- completion behind. status and completed_at state one fact.
	    completed_at = CASE WHEN enrolments.status IN ('expired', 'cancelled')
	                        THEN NULL ELSE enrolments.completed_at END,
	    -- A purchase is not overwritten. Granting or importing somebody who already
	    -- bought the course would relabel their enrolment, and the label is what says
	    -- they may not simply cancel it and keep neither the course nor their money.
	    source     = CASE WHEN enrolments.source = 'purchase'
	                      THEN enrolments.source ELSE EXCLUDED.source END,
	    expires_at = EXCLUDED.expires_at,
	    updated_at = now()
	RETURNING id, course_id, user_id, status, source, expires_at, enrolled_at, completed_at,
	          (xmax = 0) AS inserted`

// Enrol creates or reactivates an enrolment. The second return value reports
// whether a new row was created, which distinguishes "welcome" from "welcome
// back" and, more usefully, tells the caller whether to audit a new enrolment.
func (r *PostgresRepository) Enrol(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID, source string, expiresAt *time.Time) (Enrolment, bool, error) {
	var (
		e        Enrolment
		inserted bool
	)
	err := tx.QueryRow(ctx, upsertEnrolmentSQL, tenantID, courseID, userID, source, expiresAt).
		Scan(&e.ID, &e.CourseID, &e.UserID, &e.Status, &e.Source,
			&e.ExpiresAt, &e.EnrolledAt, &e.CompletedAt, &inserted)
	if err != nil {
		return Enrolment{}, false, fmt.Errorf("enroll: enrol: %w", err)
	}
	return e, inserted, nil
}

const enrolmentSQL = `
	SELECT id, course_id, user_id, status, source, expires_at, enrolled_at, completed_at
	FROM enrolments
	WHERE tenant_id = $1 AND course_id = $2 AND user_id = $3`

// Enrolment loads one enrolment.
func (r *PostgresRepository) Enrolment(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) (Enrolment, error) {
	var e Enrolment
	err := tx.QueryRow(ctx, enrolmentSQL, tenantID, courseID, userID).
		Scan(&e.ID, &e.CourseID, &e.UserID, &e.Status, &e.Source,
			&e.ExpiresAt, &e.EnrolledAt, &e.CompletedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Enrolment{}, ErrNotEnrolled
		}
		return Enrolment{}, fmt.Errorf("enroll: load enrolment: %w", err)
	}
	return e, nil
}

// CancelEnrolment ends access without destroying progress.
func (r *PostgresRepository) CancelEnrolment(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) error {
	tag, err := tx.Exec(ctx,
		`UPDATE enrolments SET status = 'cancelled', updated_at = now()
		 WHERE tenant_id = $1 AND course_id = $2 AND user_id = $3 AND status <> 'cancelled'`,
		tenantID, courseID, userID)
	if err != nil {
		return fmt.Errorf("enroll: cancel enrolment: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotEnrolled
	}
	return nil
}

// lessonForReaderSQL fetches a lesson, its course, and — in the same round trip —
// everything needed to decide whether this reader may see it.
//
// One query, not three. The access check runs on every lesson read, and a
// sequence of "load lesson, load course, load enrolment" is three round trips on
// the hottest path in the product.
//
// The LEFT JOINs mean a stranger reading a preview costs exactly what an enrolled
// learner costs.
// The drip columns ride along. The correlated subquery that counts unfinished
// earlier lessons runs only in sequential mode — the CASE short-circuits, so a
// course that does not drip pays nothing for the feature, and one that drips by
// date pays nothing for the mode it is not in.
//
// `(pt.position, pl.position, pl.id) < (t.position, l.position, l.id)` is a row
// comparison, and it is exactly the curriculum's own order: topics by position,
// lessons by position within a topic, id to break a tie that a deferred
// constraint allows mid-reorder.
const lessonForReaderSQL = `
	SELECT l.id, l.topic_id, t.course_id,
	       l.title, l.content_type, l.content, l.video_source, l.video_url, l.video_embed_url,
	       l.duration_seconds, l.is_preview, l.position,
	       l.available_at, l.available_after_days,
	       c.status, c.drip_mode,
	       e.status, e.expires_at, e.enrolled_at,
	       lp.completed_at,
	       CASE WHEN c.drip_mode = 'sequential' THEN (
	           SELECT count(*)
	           FROM lessons pl
	           JOIN topics pt ON pt.id = pl.topic_id AND pt.tenant_id = pl.tenant_id
	           LEFT JOIN lesson_progress plp
	                  ON plp.tenant_id = pl.tenant_id
	                 AND plp.lesson_id = pl.id
	                 AND plp.user_id = $3
	                 AND plp.completed_at IS NOT NULL
	           WHERE pl.tenant_id = l.tenant_id
	             AND pt.course_id = c.id
	             AND (pt.position, pl.position, pl.id) < (t.position, l.position, l.id)
	             AND plp.id IS NULL
	       ) ELSE 0 END AS prior_incomplete
	FROM lessons l
	JOIN topics  t ON t.id = l.topic_id AND t.tenant_id = l.tenant_id
	JOIN courses c ON c.id = t.course_id AND c.tenant_id = l.tenant_id
	LEFT JOIN enrolments e
	       ON e.tenant_id = l.tenant_id AND e.course_id = c.id AND e.user_id = $3
	LEFT JOIN lesson_progress lp
	       ON lp.tenant_id = l.tenant_id AND lp.lesson_id = l.id AND lp.user_id = $3
	WHERE l.tenant_id = $1 AND l.id = $2`

// LessonView is what LessonForReader returns: the lesson, plus the facts needed
// to decide access. The decision itself belongs to the service.
type LessonView struct {
	Lesson LessonContent

	CourseStatus     string
	EnrolmentStatus  *string
	EnrolmentExpires *time.Time

	// DripMode is the course's, because it decides which of the per-lesson columns
	// below means anything.
	DripMode string

	// AvailableAfterDays is the lesson's delay, read only in after_enrolment mode.
	AvailableAfterDays *int

	// EnrolledAt is this reader's own enrolment date, which after_enrolment mode
	// counts from. Nil for a reader who is not enrolled.
	EnrolledAt *time.Time

	// PriorIncomplete counts the lessons before this one, in curriculum order,
	// that the reader has not finished. Sequential mode opens a lesson when it
	// reaches zero. Computed only in that mode; zero otherwise.
	PriorIncomplete int
}

// LessonForReader loads a lesson together with the reader's enrolment and
// progress, in one query.
func (r *PostgresRepository) LessonForReader(ctx context.Context, tx pgx.Tx, tenantID, lessonID, userID uuid.UUID) (LessonView, error) {
	var v LessonView

	// uuid.Nil for an anonymous reader: it matches no enrolment and no progress,
	// so the LEFT JOINs yield nulls and the query needs no second form.
	err := tx.QueryRow(ctx, lessonForReaderSQL, tenantID, lessonID, userID).Scan(
		&v.Lesson.ID, &v.Lesson.TopicID, &v.Lesson.CourseID,
		&v.Lesson.Title, &v.Lesson.ContentType, &v.Lesson.Content,
		&v.Lesson.VideoSource, &v.Lesson.VideoURL, &v.Lesson.VideoEmbedURL,
		&v.Lesson.DurationSeconds, &v.Lesson.IsPreview, &v.Lesson.Position,
		&v.Lesson.AvailableAt, &v.AvailableAfterDays,
		&v.CourseStatus, &v.DripMode,
		&v.EnrolmentStatus, &v.EnrolmentExpires, &v.EnrolledAt,
		&v.Lesson.CompletedAt,
		&v.PriorIncomplete)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return LessonView{}, ErrNotFound
		}
		return LessonView{}, fmt.Errorf("enroll: load lesson: %w", err)
	}
	return v, nil
}

// markLessonSQL records a completion idempotently.
//
// Completing a lesson twice must not move the timestamp: "when did you finish
// this" is a fact, and clicking the button again does not change it.
const markLessonSQL = `
	INSERT INTO lesson_progress (tenant_id, user_id, lesson_id, course_id, completed_at, last_seen_at)
	VALUES ($1, $2, $3, $4, CASE WHEN $5 THEN now() ELSE NULL END, now())
	ON CONFLICT (tenant_id, user_id, lesson_id) DO UPDATE
	SET completed_at = CASE
	        WHEN $5 AND lesson_progress.completed_at IS NULL THEN now()
	        WHEN $5 THEN lesson_progress.completed_at
	        ELSE NULL
	    END,
	    last_seen_at = now(),
	    updated_at   = now()`

// MarkLesson sets or clears a lesson's completion for one learner.
func (r *PostgresRepository) MarkLesson(ctx context.Context, tx pgx.Tx, tenantID, userID, lessonID, courseID uuid.UUID, complete bool) error {
	_, err := tx.Exec(ctx, markLessonSQL, tenantID, userID, lessonID, courseID, complete)
	if err != nil {
		return fmt.Errorf("enroll: mark lesson: %w", err)
	}
	return nil
}

// recomputeProgressSQL rebuilds one learner's standing in one course.
//
// A single statement. Computing this on read would count every lesson and every
// completion on every request; a trigger would hide it from the code that causes
// it. This runs in the transaction that changed the lesson, so the roll-up can
// never disagree with the rows it summarises.
const recomputeProgressSQL = `
	INSERT INTO course_progress (tenant_id, user_id, course_id, lessons_completed, lessons_total, percent, updated_at)
	SELECT $1, $2, $3, s.done, s.total,
	       CASE WHEN s.total = 0 THEN 0 ELSE (s.done * 100 / s.total) END,
	       now()
	FROM (
	    SELECT count(*) AS total,
	           count(lp.completed_at) AS done
	    FROM lessons l
	    JOIN topics t ON t.id = l.topic_id AND t.tenant_id = l.tenant_id
	    LEFT JOIN lesson_progress lp
	           ON lp.tenant_id = l.tenant_id AND lp.lesson_id = l.id AND lp.user_id = $2
	    WHERE l.tenant_id = $1 AND t.course_id = $3
	) s
	ON CONFLICT (tenant_id, user_id, course_id) DO UPDATE
	SET lessons_completed = EXCLUDED.lessons_completed,
	    lessons_total     = EXCLUDED.lessons_total,
	    percent           = EXCLUDED.percent,
	    updated_at        = now()
	RETURNING course_id, lessons_completed, lessons_total, percent, updated_at`

// RecomputeProgress rebuilds and returns a learner's progress in a course.
func (r *PostgresRepository) RecomputeProgress(ctx context.Context, tx pgx.Tx, tenantID, userID, courseID uuid.UUID) (Progress, error) {
	var p Progress
	err := tx.QueryRow(ctx, recomputeProgressSQL, tenantID, userID, courseID).
		Scan(&p.CourseID, &p.LessonsCompleted, &p.LessonsTotal, &p.Percent, &p.UpdatedAt)
	if err != nil {
		return Progress{}, fmt.Errorf("enroll: recompute progress: %w", err)
	}
	return p, nil
}

// ProgressFor loads a learner's standing without recomputing it.
func (r *PostgresRepository) ProgressFor(ctx context.Context, tx pgx.Tx, tenantID, userID, courseID uuid.UUID) (Progress, error) {
	var p Progress
	err := tx.QueryRow(ctx,
		`SELECT course_id, lessons_completed, lessons_total, percent, updated_at
		 FROM course_progress WHERE tenant_id = $1 AND user_id = $2 AND course_id = $3`,
		tenantID, userID, courseID).
		Scan(&p.CourseID, &p.LessonsCompleted, &p.LessonsTotal, &p.Percent, &p.UpdatedAt)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No row means nothing has been completed yet, which is progress of zero,
			// not an error.
			return Progress{CourseID: courseID}, nil
		}
		return Progress{}, fmt.Errorf("enroll: load progress: %w", err)
	}
	return p, nil
}

// CompleteEnrolment marks an enrolment finished, once.
func (r *PostgresRepository) CompleteEnrolment(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) (bool, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE enrolments SET status = 'completed', completed_at = now(), updated_at = now()
		 WHERE tenant_id = $1 AND course_id = $2 AND user_id = $3 AND status <> 'completed'`,
		tenantID, courseID, userID)
	if err != nil {
		return false, fmt.Errorf("enroll: complete enrolment: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ReopenEnrolment takes a completed enrolment back to active, reporting whether
// it changed anything.
//
// completed_at is cleared with the status. The two are one fact stated twice, and
// a row where the status says active while the timestamp says finished is a row
// that will be read whichever way the reader happens to look.
func (r *PostgresRepository) ReopenEnrolment(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) (bool, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE enrolments SET status = 'active', completed_at = NULL, updated_at = now()
		 WHERE tenant_id = $1 AND course_id = $2 AND user_id = $3 AND status = 'completed'`,
		tenantID, courseID, userID)
	if err != nil {
		return false, fmt.Errorf("enroll: reopen enrolment: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// listEnrolmentsSQL is a learner's dashboard: every enrolment, its course, and
// its progress, in one joined query rather than one query per enrolment.
const listEnrolmentsSQL = `
	SELECT e.id, e.course_id, e.user_id, e.status, e.source,
	       e.expires_at, e.enrolled_at, e.completed_at,
	       c.slug, c.title,
	       coalesce(cp.lessons_completed, 0), coalesce(cp.lessons_total, 0),
	       coalesce(cp.percent, 0), coalesce(cp.updated_at, e.enrolled_at)
	FROM enrolments e
	JOIN courses c ON c.id = e.course_id AND c.tenant_id = e.tenant_id
	LEFT JOIN course_progress cp
	       ON cp.tenant_id = e.tenant_id AND cp.user_id = e.user_id AND cp.course_id = e.course_id
	WHERE e.tenant_id = $1 AND e.user_id = $2
	ORDER BY e.enrolled_at DESC, e.id DESC
	LIMIT $3`

// ListEnrolments returns a learner's enrolments, newest first.
func (r *PostgresRepository) ListEnrolments(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, limit int) ([]EnrolmentWithCourse, error) {
	rows, err := tx.Query(ctx, listEnrolmentsSQL, tenantID, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("enroll: list enrolments: %w", err)
	}
	defer rows.Close()

	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (EnrolmentWithCourse, error) {
		var e EnrolmentWithCourse
		err := row.Scan(
			&e.Enrolment.ID, &e.Enrolment.CourseID, &e.Enrolment.UserID,
			&e.Enrolment.Status, &e.Enrolment.Source,
			&e.Enrolment.ExpiresAt, &e.Enrolment.EnrolledAt, &e.Enrolment.CompletedAt,
			&e.CourseSlug, &e.CourseTitle,
			&e.Progress.LessonsCompleted, &e.Progress.LessonsTotal, &e.Progress.Percent, &e.Progress.UpdatedAt)
		e.Progress.CourseID = e.Enrolment.CourseID
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("enroll: scan enrolments: %w", err)
	}
	return out, nil
}

// CountEnrolments reports how many learners a course has. Used by instructors.
func (r *PostgresRepository) CountEnrolments(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) (int, error) {
	var n int
	err := tx.QueryRow(ctx,
		`SELECT count(*) FROM enrolments
		 WHERE tenant_id = $1 AND course_id = $2 AND status IN ('active', 'completed')`,
		tenantID, courseID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("enroll: count enrolments: %w", err)
	}
	return n, nil
}

/*
CourseFacts counts the learners and the reviews for a page of courses at once.

Two aggregates in one statement, both keyed by course, both index-backed: the
enrolment count walks enrolments_course_idx and the rating walks
course_reviews_one_per_learner_idx. A course nobody has enrolled on or reviewed
simply has no row here, and the caller reads a missing key as the zero — which is
"no learners, unrated", and is the truth.

The `= ANY($2)` is the whole point. The catalogue draws twenty of these on a page
and a count per card is the N+1 this codebase does not write.
*/
const courseFactsSQL = `
	SELECT c.id,
	       coalesce(e.learners, 0),
	       coalesce(r.average, 0),
	       coalesce(r.reviews, 0)
	FROM courses c
	LEFT JOIN LATERAL (
	    SELECT count(*) AS learners
	    FROM enrolments e
	    WHERE e.tenant_id = c.tenant_id AND e.course_id = c.id
	      AND e.status IN ('active', 'completed')
	) e ON true
	LEFT JOIN LATERAL (
	    SELECT avg(rating)::float8 AS average, count(*) AS reviews
	    FROM course_reviews rv
	    WHERE rv.tenant_id = c.tenant_id AND rv.course_id = c.id
	) r ON true
	WHERE c.tenant_id = $1 AND c.id = ANY($2)`

// CourseFacts returns, for each course id given, its learner count and its rating.
func (r *PostgresRepository) CourseFacts(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, courseIDs []uuid.UUID) (map[uuid.UUID]CourseFacts, error) {
	rows, err := tx.Query(ctx, courseFactsSQL, tenantID, courseIDs)
	if err != nil {
		return nil, fmt.Errorf("enroll: course facts: %w", err)
	}
	defer rows.Close()

	facts := make(map[uuid.UUID]CourseFacts, len(courseIDs))
	for rows.Next() {
		var (
			id uuid.UUID
			f  CourseFacts
		)
		if err := rows.Scan(&id, &f.Learners, &f.RatingAverage, &f.RatingCount); err != nil {
			return nil, fmt.Errorf("enroll: scan course facts: %w", err)
		}
		facts[id] = f
	}
	return facts, rows.Err()
}

const upsertReviewSQL = `
	INSERT INTO course_reviews (tenant_id, course_id, user_id, rating, body)
	VALUES ($1, $2, $3, $4, $5)
	ON CONFLICT (tenant_id, course_id, user_id) DO UPDATE
	SET rating = EXCLUDED.rating, body = EXCLUDED.body, updated_at = now()
	RETURNING id, rating, body, created_at, updated_at`

// UpsertReview writes a learner's review, editing their prior one if present.
func (r *PostgresRepository) UpsertReview(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID, rating int, body string) (Review, error) {
	rev := Review{CourseID: courseID, UserID: userID}
	err := tx.QueryRow(ctx, upsertReviewSQL, tenantID, courseID, userID, rating, body).
		Scan(&rev.ID, &rev.Rating, &rev.Body, &rev.CreatedAt, &rev.UpdatedAt)
	if err != nil {
		return Review{}, fmt.Errorf("enroll: upsert review: %w", err)
	}
	return rev, nil
}

// DeleteReview removes a learner's review. Absent is not an error here.
func (r *PostgresRepository) DeleteReview(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) error {
	_, err := tx.Exec(ctx,
		`DELETE FROM course_reviews WHERE tenant_id = $1 AND course_id = $2 AND user_id = $3`,
		tenantID, courseID, userID)
	if err != nil {
		return fmt.Errorf("enroll: delete review: %w", err)
	}
	return nil
}

const reviewForSQL = `
	SELECT r.id, r.rating, r.body, COALESCE(u.name, ''), r.created_at, r.updated_at
	FROM course_reviews r
	LEFT JOIN users u ON u.id = r.user_id
	WHERE r.tenant_id = $1 AND r.course_id = $2 AND r.user_id = $3`

// ReviewFor returns one learner's review, or ErrReviewNotFound.
func (r *PostgresRepository) ReviewFor(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) (Review, error) {
	rev := Review{CourseID: courseID, UserID: userID}
	err := tx.QueryRow(ctx, reviewForSQL, tenantID, courseID, userID).
		Scan(&rev.ID, &rev.Rating, &rev.Body, &rev.AuthorName, &rev.CreatedAt, &rev.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Review{}, ErrReviewNotFound
		}
		return Review{}, fmt.Errorf("enroll: load review: %w", err)
	}
	return rev, nil
}

const ListReviewsSQL = `
	SELECT r.id, r.user_id, r.rating, r.body, COALESCE(u.name, ''), r.created_at, r.updated_at
	FROM course_reviews r
	LEFT JOIN users u ON u.id = r.user_id
	WHERE r.tenant_id = $1 AND r.course_id = $2
	ORDER BY r.created_at DESC, r.id
	LIMIT $3`

// ListReviews returns a course's reviews, newest first, with author names.
func (r *PostgresRepository) ListReviews(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, limit int) ([]Review, error) {
	rows, err := tx.Query(ctx, ListReviewsSQL, tenantID, courseID, limit)
	if err != nil {
		return nil, fmt.Errorf("enroll: list reviews: %w", err)
	}
	defer rows.Close()

	var out []Review
	for rows.Next() {
		rev := Review{CourseID: courseID}
		if err := rows.Scan(&rev.ID, &rev.UserID, &rev.Rating, &rev.Body, &rev.AuthorName, &rev.CreatedAt, &rev.UpdatedAt); err != nil {
			return nil, fmt.Errorf("enroll: scan review: %w", err)
		}
		out = append(out, rev)
	}
	return out, rows.Err()
}

// ReviewSummary returns a course's review count and mean rating.
func (r *PostgresRepository) ReviewSummary(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) (ReviewSummary, error) {
	var s ReviewSummary
	err := tx.QueryRow(ctx,
		`SELECT count(*), COALESCE(avg(rating), 0) FROM course_reviews WHERE tenant_id = $1 AND course_id = $2`,
		tenantID, courseID).Scan(&s.Count, &s.Average)
	if err != nil {
		return ReviewSummary{}, fmt.Errorf("enroll: review summary: %w", err)
	}
	return s, nil
}

// CourseStatsSQL is exported for a query-plan test: the per-course analytics must
// seek enrolments on (tenant_id, course_id), never scan the whole table.
const CourseStatsSQL = `
	SELECT
		count(*),
		count(*) FILTER (WHERE e.status = 'active'),
		count(*) FILTER (WHERE e.status = 'completed'),
		count(*) FILTER (WHERE e.status IN ('expired', 'cancelled')),
		COALESCE(avg(p.percent) FILTER (WHERE e.status IN ('active', 'completed')), 0)
	FROM enrolments e
	LEFT JOIN course_progress p
		ON p.tenant_id = e.tenant_id AND p.user_id = e.user_id AND p.course_id = e.course_id
	WHERE e.tenant_id = $1 AND e.course_id = $2`

// CourseStats totals a course's enrolments by status and averages live progress,
// in one aggregate query. Reviews are summed separately by the caller.
func (r *PostgresRepository) CourseStats(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) (CourseAnalytics, error) {
	var a CourseAnalytics
	err := tx.QueryRow(ctx, CourseStatsSQL, tenantID, courseID).
		Scan(&a.Total, &a.Active, &a.Completed, &a.Inactive, &a.AvgProgress)
	if err != nil {
		return CourseAnalytics{}, fmt.Errorf("enroll: course stats: %w", err)
	}
	return a, nil
}

/*
BulkEnrol enrols a whole cohort in one statement.

`unnest($3::uuid[]) WITH ORDINALITY` and not a loop: a school importing four
hundred learners would otherwise be four hundred round trips inside one
transaction, holding a pooled connection for the length of all of them. The
conflict clause is the same one a single enrolment uses — reactivating a lapsed
row, and never relabelling a purchase — so an import over an existing cohort
cannot mean something different from enrolling them one at a time.

It returns what happened to each, because "we enrolled 400" is not what the person
who pasted the list needs to know: they need the four that were already there.
*/
func (r *PostgresRepository) BulkEnrol(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, userIDs []uuid.UUID, source string) (map[uuid.UUID]bool, error) {
	rows, err := tx.Query(ctx,
		`INSERT INTO enrolments (tenant_id, course_id, user_id, source)
		 SELECT $1, $2, u, $4
		 FROM unnest($3::uuid[]) AS u
		 ON CONFLICT (tenant_id, course_id, user_id) DO UPDATE
		 SET status       = CASE WHEN enrolments.status IN ('expired', 'cancelled')
		                         THEN 'active' ELSE enrolments.status END,
		     completed_at = CASE WHEN enrolments.status IN ('expired', 'cancelled')
		                         THEN NULL ELSE enrolments.completed_at END,
		     source       = CASE WHEN enrolments.source = 'purchase'
		                         THEN enrolments.source ELSE EXCLUDED.source END,
		     updated_at   = now()
		 RETURNING user_id, (xmax = 0) AS inserted`,
		tenantID, courseID, userIDs, source)
	if err != nil {
		return nil, fmt.Errorf("enroll: bulk enrol: %w", err)
	}
	defer rows.Close()

	added := make(map[uuid.UUID]bool, len(userIDs))
	for rows.Next() {
		var (
			userID   uuid.UUID
			inserted bool
		)
		if err := rows.Scan(&userID, &inserted); err != nil {
			return nil, fmt.Errorf("enroll: scan bulk enrolment: %w", err)
		}
		added[userID] = inserted
	}
	return added, rows.Err()
}
