package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgresRepository satisfies Repository. Every method takes the pgx.Tx handed
// to it by database.WithTenant, so no query can escape the tenant binding.
type PostgresRepository struct{}

// NewPostgresRepository returns a Repository backed by Postgres.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

// listCoursesSQL is a keyset page over (created_at, id) descending.
//
// The row-comparison `(created_at, id) < ($2, $3)` is a single index seek on
// courses_tenant_status_created_idx, which covers the filter and the ordering.
// The plan is an index scan with no sort node, so page 500 costs what page 1
// costs.
//
// It fetches limit+1 rows: the extra row answers "is there a next page" without
// a COUNT(*), which would scan every matching row on every request.
const listPublishedCoursesSQL = `
	SELECT id, slug, title, summary, difficulty, status, published_at, drip_mode, created_at, updated_at
	FROM courses
	WHERE tenant_id = $1
	  AND status = 'published'
	  AND ($2::timestamptz IS NULL OR (created_at, id) < ($2, $3))
	ORDER BY created_at DESC, id DESC
	LIMIT $4`

// listAllCoursesSQL is the same page without the status predicate.
//
// It is a separate statement rather than `status = 'published' OR $4`, because
// that predicate is not sargable: the planner would abandon
// courses_tenant_status_created_idx even when the flag is false, and every
// anonymous catalog request would pay for a feature only authors use. Each
// statement gets an index that covers its filter and its sort.
const listAllCoursesSQL = `
	SELECT id, slug, title, summary, difficulty, status, published_at, drip_mode, created_at, updated_at
	FROM courses
	WHERE tenant_id = $1
	  AND ($2::timestamptz IS NULL OR (created_at, id) < ($2, $3))
	ORDER BY created_at DESC, id DESC
	LIMIT $4`

// ListCourses returns one keyset page.
//
// Drafts are excluded by the statement, not filtered out of its result. A draft
// that is loaded and then discarded has already been loaded, and the first
// refactor that forgets the discard publishes it.
func (r *PostgresRepository) ListCourses(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, p ListParams) ([]Course, error) {
	var (
		afterTime any
		afterID   any
	)
	if p.Cursor != "" {
		c, err := decodeCursor(p.Cursor)
		if err != nil {
			return nil, err
		}
		afterTime, afterID = c.CreatedAt, c.ID
	}

	query := listPublishedCoursesSQL
	if p.IncludeDrafts {
		query = listAllCoursesSQL
	}

	rows, err := tx.Query(ctx, query, tenantID, afterTime, afterID, p.Limit+1)
	if err != nil {
		return nil, fmt.Errorf("catalog: list courses: %w", err)
	}
	defer rows.Close()

	courses, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Course, error) {
		var c Course
		err := row.Scan(&c.ID, &c.Slug, &c.Title, &c.Summary, &c.Difficulty,
			&c.Status, &c.PublishedAt, &c.DripMode, &c.CreatedAt, &c.UpdatedAt)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("catalog: scan courses: %w", err)
	}
	return courses, nil
}

// courseBySlugSQL hides unpublished courses unless the caller may see drafts.
//
// The filter is in the query, not in a check after the fact. A draft that is
// loaded and then discarded has already been loaded, and the first refactor that
// forgets the discard turns an unreleased course into a public one.
const courseBySlugSQL = `
	SELECT id, slug, title, summary, difficulty, status, published_at, drip_mode, created_at, updated_at
	FROM courses
	WHERE tenant_id = $1 AND lower(slug) = lower($2)
	  AND (status = 'published' OR $3)`

// CourseBySlug loads a single course. The unique index on (tenant_id, lower(slug))
// makes this an index lookup.
//
// includeDrafts must come from an authorisation decision, never from a request
// parameter.
func (r *PostgresRepository) CourseBySlug(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string, includeDrafts bool) (Course, error) {
	var c Course
	err := tx.QueryRow(ctx, courseBySlugSQL, tenantID, slug, includeDrafts).Scan(
		&c.ID, &c.Slug, &c.Title, &c.Summary, &c.Difficulty,
		&c.Status, &c.PublishedAt, &c.DripMode, &c.CreatedAt, &c.UpdatedAt)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Course{}, ErrNotFound
		}
		return Course{}, fmt.Errorf("catalog: load course %q: %w", slug, err)
	}
	return c, nil
}

const topicsByCourseSQL = `
	SELECT id, course_id, title, position
	FROM topics
	WHERE tenant_id = $1 AND course_id = $2
	ORDER BY position, id`

// lessonsByTopicsSQL fetches the lessons of every topic in one round trip.
//
// This is the query that prevents the N+1. The obvious implementation loops over
// topics and queries lessons for each, issuing one query per topic — invisible on
// a three-topic fixture and catastrophic on a forty-topic course under load.
// `topic_id = ANY($2)` collapses that to a single index scan, and the ORDER BY
// means the rows arrive grouped and ordered, so the service never sorts in Go.
const lessonsByTopicsSQL = `
	SELECT id, topic_id, title, content_type, duration_seconds, is_preview, position
	FROM lessons
	WHERE tenant_id = $1 AND topic_id = ANY($2)
	ORDER BY topic_id, position, id`

// CurriculumFor loads every topic and lesson of a course in exactly two queries,
// regardless of how many topics or lessons exist.
func (r *PostgresRepository) CurriculumFor(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) ([]Topic, error) {
	rows, err := tx.Query(ctx, topicsByCourseSQL, tenantID, courseID)
	if err != nil {
		return nil, fmt.Errorf("catalog: list topics: %w", err)
	}
	defer rows.Close()

	topics, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Topic, error) {
		var t Topic
		err := row.Scan(&t.ID, &t.CourseID, &t.Title, &t.Position)
		return t, err
	})
	if err != nil {
		return nil, fmt.Errorf("catalog: scan topics: %w", err)
	}

	if len(topics) == 0 {
		// No topics means no lessons. Skipping the second query is not an
		// optimisation, it is correctness: `= ANY('{}')` would match nothing anyway,
		// but issuing the round trip to learn that is waste.
		return topics, nil
	}

	topicIDs := make([]uuid.UUID, len(topics))
	for i, t := range topics {
		topicIDs[i] = t.ID
	}

	lessonRows, err := tx.Query(ctx, lessonsByTopicsSQL, tenantID, topicIDs)
	if err != nil {
		return nil, fmt.Errorf("catalog: list lessons: %w", err)
	}
	defer lessonRows.Close()

	lessons, err := pgx.CollectRows(lessonRows, func(row pgx.CollectableRow) (Lesson, error) {
		var l Lesson
		err := row.Scan(&l.ID, &l.TopicID, &l.Title, &l.ContentType,
			&l.DurationSeconds, &l.IsPreview, &l.Position)
		return l, err
	})
	if err != nil {
		return nil, fmt.Errorf("catalog: scan lessons: %w", err)
	}

	// Stitch the two result sets in one pass. The index on the lesson side means
	// they arrive already ordered within each topic, so appending preserves order.
	byTopic := make(map[uuid.UUID][]Lesson, len(topics))
	for _, l := range lessons {
		byTopic[l.TopicID] = append(byTopic[l.TopicID], l)
	}
	for i := range topics {
		topics[i].Lessons = byTopic[topics[i].ID]
	}

	return topics, nil
}
