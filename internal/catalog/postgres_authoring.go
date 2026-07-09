package catalog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// appendTopicSQL places the new topic after the last existing one.
//
// The position is computed in the same statement as the insert, so two
// concurrent appends cannot both read the same max and collide. The deferred
// unique constraint on (tenant_id, course_id, position) catches it if they do.
const appendTopicSQL = `
	INSERT INTO topics (tenant_id, course_id, title, position)
	SELECT $1, $2, $3, coalesce(max(position) + 1, 0)
	FROM topics WHERE tenant_id = $1 AND course_id = $2
	RETURNING id, course_id, title, position`

// CreateTopic appends a topic.
func (r *PostgresRepository) CreateTopic(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, n NewTopic) (Topic, error) {
	var t Topic
	err := tx.QueryRow(ctx, appendTopicSQL, tenantID, courseID, n.Title).
		Scan(&t.ID, &t.CourseID, &t.Title, &t.Position)
	if err != nil {
		return Topic{}, fmt.Errorf("catalog: create topic: %w", err)
	}
	return t, nil
}

// UpdateTopic applies a patch. COALESCE leaves a nil field untouched, so the
// caller distinguishes "clear this" from "do not touch this".
func (r *PostgresRepository) UpdateTopic(ctx context.Context, tx pgx.Tx, tenantID, topicID uuid.UUID, p TopicPatch) (Topic, error) {
	var t Topic
	err := tx.QueryRow(ctx,
		`UPDATE topics SET title = coalesce($3, title), updated_at = now()
		 WHERE tenant_id = $1 AND id = $2
		 RETURNING id, course_id, title, position`,
		tenantID, topicID, p.Title).
		Scan(&t.ID, &t.CourseID, &t.Title, &t.Position)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Topic{}, ErrNotFound
		}
		return Topic{}, fmt.Errorf("catalog: update topic: %w", err)
	}
	return t, nil
}

// deleteTopicSQL removes a topic and closes the gap, in one round trip.
//
// Positions stay dense. A gap is harmless until the first time somebody treats
// position as an index, and then it is an off-by-one nobody can reproduce.
const deleteTopicSQL = `
	WITH removed AS (
		DELETE FROM topics WHERE tenant_id = $1 AND id = $2
		RETURNING course_id, position
	), closed AS (
		UPDATE topics t SET position = t.position - 1
		FROM removed r
		WHERE t.tenant_id = $1 AND t.course_id = r.course_id AND t.position > r.position
	)
	SELECT course_id FROM removed`

// DeleteTopic removes a topic, cascading its lessons, and returns its course.
func (r *PostgresRepository) DeleteTopic(ctx context.Context, tx pgx.Tx, tenantID, topicID uuid.UUID) (uuid.UUID, error) {
	var courseID uuid.UUID
	err := tx.QueryRow(ctx, deleteTopicSQL, tenantID, topicID).Scan(&courseID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrNotFound
		}
		return uuid.Nil, fmt.Errorf("catalog: delete topic: %w", err)
	}
	return courseID, nil
}

// reorderTopicsSQL rewrites every position from the submitted order, in one
// statement.
//
// `unnest(...) WITH ORDINALITY` turns the array into (id, rank) pairs, so the
// new position of each topic is its index in the list. The unique constraint on
// (tenant_id, course_id, position) is DEFERRABLE precisely because a reorder
// necessarily passes through states where two rows share a position.
const reorderTopicsSQL = `
	UPDATE topics t SET position = v.rank - 1, updated_at = now()
	FROM unnest($3::uuid[]) WITH ORDINALITY AS v(id, rank)
	WHERE t.tenant_id = $1 AND t.course_id = $2 AND t.id = v.id`

// ReorderTopics sets the order of a course's topics.
//
// The submitted list must name every topic exactly once. A shorter list would
// leave the unnamed topics wherever they were, producing an order the author
// never asked for; a list naming a foreign topic would silently do nothing to
// it. Both are refused rather than half-applied.
func (r *PostgresRepository) ReorderTopics(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, order []uuid.UUID) error {
	if err := checkComplete(ctx, tx,
		`SELECT count(*) FROM topics WHERE tenant_id = $1 AND course_id = $2`,
		tenantID, courseID, order); err != nil {
		return err
	}

	tag, err := tx.Exec(ctx, reorderTopicsSQL, tenantID, courseID, order)
	if err != nil {
		return fmt.Errorf("catalog: reorder topics: %w", err)
	}
	if int(tag.RowsAffected()) != len(order) {
		// An id in the list did not belong to this course. The transaction rolls
		// back rather than apply a partial order.
		return fmt.Errorf("%w: %d of %d topics matched", ErrIncompleteOrder, tag.RowsAffected(), len(order))
	}
	return nil
}

const appendLessonSQL = `
	INSERT INTO lessons (tenant_id, topic_id, title, content_type, content, video_source, video_url,
	                     duration_seconds, is_preview, position)
	SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, coalesce(max(position) + 1, 0)
	FROM lessons WHERE tenant_id = $1 AND topic_id = $2
	RETURNING id, topic_id, title, content_type, duration_seconds, is_preview, position`

// CreateLesson appends a lesson to a topic.
func (r *PostgresRepository) CreateLesson(ctx context.Context, tx pgx.Tx, tenantID, topicID uuid.UUID, n NewLesson) (Lesson, error) {
	var l Lesson
	err := tx.QueryRow(ctx, appendLessonSQL, tenantID, topicID, n.Title, n.ContentType,
		n.Content, n.VideoSource, n.VideoURL, n.DurationSeconds, n.IsPreview).
		Scan(&l.ID, &l.TopicID, &l.Title, &l.ContentType, &l.DurationSeconds, &l.IsPreview, &l.Position)

	if err != nil {
		// A topic that does not exist, or belongs to another tenant, produces no row
		// for the max() subquery and then a foreign-key violation.
		if isForeignKeyViolation(err) {
			return Lesson{}, ErrNotFound
		}
		return Lesson{}, fmt.Errorf("catalog: create lesson: %w", err)
	}
	return l, nil
}

// UpdateLesson applies a patch.
func (r *PostgresRepository) UpdateLesson(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID, p LessonPatch) (Lesson, error) {
	var l Lesson
	err := tx.QueryRow(ctx,
		`UPDATE lessons SET
		     title            = coalesce($3, title),
		     content_type     = coalesce($4, content_type),
		     content          = coalesce($5, content),
		     video_source     = coalesce($6, video_source),
		     video_url        = coalesce($7, video_url),
		     duration_seconds = coalesce($8, duration_seconds),
		     is_preview       = coalesce($9, is_preview),
		     updated_at       = now()
		 WHERE tenant_id = $1 AND id = $2
		 RETURNING id, topic_id, title, content_type, duration_seconds, is_preview, position`,
		tenantID, lessonID, p.Title, p.ContentType, p.Content, p.VideoSource, p.VideoURL,
		p.DurationSeconds, p.IsPreview).
		Scan(&l.ID, &l.TopicID, &l.Title, &l.ContentType, &l.DurationSeconds, &l.IsPreview, &l.Position)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Lesson{}, ErrNotFound
		}
		return Lesson{}, fmt.Errorf("catalog: update lesson: %w", err)
	}
	return l, nil
}

const deleteLessonSQL = `
	WITH removed AS (
		DELETE FROM lessons WHERE tenant_id = $1 AND id = $2
		RETURNING topic_id, position
	), closed AS (
		UPDATE lessons l SET position = l.position - 1
		FROM removed r
		WHERE l.tenant_id = $1 AND l.topic_id = r.topic_id AND l.position > r.position
	)
	SELECT topic_id FROM removed`

// DeleteLesson removes a lesson and closes the gap.
func (r *PostgresRepository) DeleteLesson(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID) (uuid.UUID, error) {
	var topicID uuid.UUID
	err := tx.QueryRow(ctx, deleteLessonSQL, tenantID, lessonID).Scan(&topicID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrNotFound
		}
		return uuid.Nil, fmt.Errorf("catalog: delete lesson: %w", err)
	}
	return topicID, nil
}

const reorderLessonsSQL = `
	UPDATE lessons l SET position = v.rank - 1, updated_at = now()
	FROM unnest($3::uuid[]) WITH ORDINALITY AS v(id, rank)
	WHERE l.tenant_id = $1 AND l.topic_id = $2 AND l.id = v.id`

// ReorderLessons sets the order of a topic's lessons.
func (r *PostgresRepository) ReorderLessons(ctx context.Context, tx pgx.Tx, tenantID, topicID uuid.UUID, order []uuid.UUID) error {
	if err := checkComplete(ctx, tx,
		`SELECT count(*) FROM lessons WHERE tenant_id = $1 AND topic_id = $2`,
		tenantID, topicID, order); err != nil {
		return err
	}

	tag, err := tx.Exec(ctx, reorderLessonsSQL, tenantID, topicID, order)
	if err != nil {
		return fmt.Errorf("catalog: reorder lessons: %w", err)
	}
	if int(tag.RowsAffected()) != len(order) {
		return fmt.Errorf("%w: %d of %d lessons matched", ErrIncompleteOrder, tag.RowsAffected(), len(order))
	}
	return nil
}

// checkComplete refuses an order that does not name every sibling exactly once.
func checkComplete(ctx context.Context, tx pgx.Tx, countSQL string, tenantID, parentID uuid.UUID, order []uuid.UUID) error {
	seen := make(map[uuid.UUID]struct{}, len(order))
	for _, id := range order {
		if _, dup := seen[id]; dup {
			return fmt.Errorf("%w: %s appears twice", ErrIncompleteOrder, id)
		}
		seen[id] = struct{}{}
	}

	var total int
	if err := tx.QueryRow(ctx, countSQL, tenantID, parentID).Scan(&total); err != nil {
		return fmt.Errorf("catalog: count siblings: %w", err)
	}
	if total == 0 {
		return ErrNotFound
	}
	if total != len(order) {
		return fmt.Errorf("%w: %d listed, %d exist", ErrIncompleteOrder, len(order), total)
	}
	return nil
}

// TopicByID loads one topic.
func (r *PostgresRepository) TopicByID(ctx context.Context, tx pgx.Tx, tenantID, topicID uuid.UUID) (Topic, error) {
	var t Topic
	err := tx.QueryRow(ctx,
		`SELECT id, course_id, title, position FROM topics WHERE tenant_id = $1 AND id = $2`,
		tenantID, topicID).Scan(&t.ID, &t.CourseID, &t.Title, &t.Position)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Topic{}, ErrNotFound
		}
		return Topic{}, fmt.Errorf("catalog: load topic: %w", err)
	}
	return t, nil
}

// CourseByID loads one course, whatever its status.
func (r *PostgresRepository) CourseByID(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) (Course, error) {
	var c Course
	err := tx.QueryRow(ctx,
		`SELECT id, slug, title, summary, difficulty, status, published_at, created_at, updated_at
		 FROM courses WHERE tenant_id = $1 AND id = $2`, tenantID, courseID).
		Scan(&c.ID, &c.Slug, &c.Title, &c.Summary, &c.Difficulty, &c.Status,
			&c.PublishedAt, &c.CreatedAt, &c.UpdatedAt)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Course{}, ErrNotFound
		}
		return Course{}, fmt.Errorf("catalog: load course: %w", err)
	}
	return c, nil
}

// CountLessons counts every lesson in a course, across its topics.
func (r *PostgresRepository) CountLessons(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) (int, error) {
	var n int
	err := tx.QueryRow(ctx,
		`SELECT count(*) FROM lessons l
		 JOIN topics t ON t.id = l.topic_id
		 WHERE l.tenant_id = $1 AND t.course_id = $2`, tenantID, courseID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("catalog: count lessons: %w", err)
	}
	return n, nil
}

// SetCourseStatus transitions a course between draft and published.
//
// published_at is stamped on the first publish and never cleared, so "when did
// this first go live" survives an unpublish. Republishing does not rewrite it.
func (r *PostgresRepository) SetCourseStatus(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, status string) (Course, error) {
	var (
		c   Course
		now = time.Now()
	)

	err := tx.QueryRow(ctx,
		`UPDATE courses SET
		     status       = $3,
		     published_at = CASE WHEN $3 = 'published' AND published_at IS NULL THEN $4 ELSE published_at END,
		     updated_at   = now()
		 WHERE tenant_id = $1 AND id = $2
		 RETURNING id, slug, title, summary, difficulty, status, published_at, created_at, updated_at`,
		tenantID, courseID, status, now).
		Scan(&c.ID, &c.Slug, &c.Title, &c.Summary, &c.Difficulty, &c.Status,
			&c.PublishedAt, &c.CreatedAt, &c.UpdatedAt)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Course{}, ErrNotFound
		}
		return Course{}, fmt.Errorf("catalog: set course status: %w", err)
	}
	return c, nil
}

func isForeignKeyViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23503"
}
