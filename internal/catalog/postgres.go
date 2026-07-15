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
// The two optional filters are the same residual predicate the cursor uses: a
// null parameter passes every row, so the index still orders the scan and the
// plan is unchanged when nobody searches or filters. Courses per workspace are
// few, so the ILIKE is a filter on an already-small, index-ordered scan, not a
// reason to reach for a trigram index.
// The author is carried as an id and resolved to a name afterwards, in one
// statement for the whole page. Joining `users` here reads like the cheaper thing
// — a primary-key lookup per row — and is not: that table's RLS policy pulls
// memberships and invitations into *this* plan, and the listing stops being the
// clean index seek a test pins it to. See instructorNames.
const listPublishedCoursesSQL = `
	SELECT id, slug, title, summary, difficulty, status, published_at, drip_mode,
	       image_key, created_by, created_at, updated_at
	FROM courses
	WHERE tenant_id = $1
	  AND status = 'published'
	  AND ($2::timestamptz IS NULL OR (created_at, id) < ($2, $3))
	  AND ($5::text IS NULL OR difficulty = $5)
	  AND ($6::text IS NULL OR title ILIKE '%' || $6 || '%')
	  AND ($7::uuid IS NULL OR created_by = $7)
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
	SELECT id, slug, title, summary, difficulty, status, published_at, drip_mode,
	       image_key, created_by, created_at, updated_at
	FROM courses
	WHERE tenant_id = $1
	  AND ($2::timestamptz IS NULL OR (created_at, id) < ($2, $3))
	  AND ($5::text IS NULL OR difficulty = $5)
	  AND ($6::text IS NULL OR title ILIKE '%' || $6 || '%')
	  AND ($7::uuid IS NULL OR created_by = $7)
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

	// Blank filters go to the database as NULL, which the query reads as "no
	// filter". A non-null empty string would match nothing, which is the opposite.
	var difficulty, search, author any
	if p.Difficulty != "" {
		difficulty = p.Difficulty
	}
	if p.Search != "" {
		search = p.Search
	}
	if p.Author != nil {
		author = *p.Author
	}

	rows, err := tx.Query(ctx, query, tenantID, afterTime, afterID, p.Limit+1, difficulty, search, author)
	if err != nil {
		return nil, fmt.Errorf("catalog: list courses: %w", err)
	}
	defer rows.Close()

	courses, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Course, error) {
		var c Course
		err := row.Scan(&c.ID, &c.Slug, &c.Title, &c.Summary, &c.Difficulty,
			&c.Status, &c.PublishedAt, &c.DripMode, &c.ImageKey, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("catalog: scan courses: %w", err)
	}

	// The lesson count is a second statement, not a subquery on the listing. A
	// subquery would drag the lessons and topics tables into the listing's plan —
	// the very plan a test pins to an index seek — and a per-course subquery is one
	// count per row besides. This counts every course on the page at once, keyed by
	// id, exactly as the children of any tree-load are batched here.
	if len(courses) > 0 {
		ids := make([]uuid.UUID, len(courses))
		for i := range courses {
			ids[i] = courses[i].ID
		}
		counts, err := r.lessonCounts(ctx, tx, tenantID, ids)
		if err != nil {
			return nil, err
		}
		for i := range courses {
			courses[i].LessonCount = counts[courses[i].ID]
		}

		names, err := r.instructorNames(ctx, tx, courses)
		if err != nil {
			return nil, err
		}
		for i := range courses {
			if courses[i].CreatedBy != nil {
				courses[i].InstructorName = names[*courses[i].CreatedBy]
			}
		}
	}
	return courses, nil
}

// instructorNames resolves the page's authors in one statement.
//
// Distinct ids, because a workspace's courses are written by a handful of people
// and a page of twenty is usually two or three names. An author since erased has
// no row, and the caller reads the missing key as the empty string — which is what
// the course page already shows for one.
func (r *PostgresRepository) instructorNames(ctx context.Context, tx pgx.Tx, courses []Course) (map[uuid.UUID]string, error) {
	seen := make(map[uuid.UUID]struct{}, len(courses))
	ids := make([]uuid.UUID, 0, len(courses))
	for _, c := range courses {
		if c.CreatedBy == nil {
			continue
		}
		if _, dup := seen[*c.CreatedBy]; dup {
			continue
		}
		seen[*c.CreatedBy] = struct{}{}
		ids = append(ids, *c.CreatedBy)
	}
	if len(ids) == 0 {
		return map[uuid.UUID]string{}, nil
	}

	rows, err := tx.Query(ctx, `SELECT id, name FROM users WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("catalog: instructor names: %w", err)
	}
	defer rows.Close()

	names := make(map[uuid.UUID]string, len(ids))
	for rows.Next() {
		var (
			id   uuid.UUID
			name string
		)
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("catalog: scan instructor name: %w", err)
		}
		names[id] = name
	}
	return names, rows.Err()
}

// lessonCountsByCourseSQL counts the lessons under a set of courses in one pass.
//
// A course with no lessons simply does not appear in the result; the caller reads
// a missing key as zero. The join walks topics_tenant_course_position_idx to the
// course's topics and lessons_tenant_topic_position_idx to their lessons.
const lessonCountsByCourseSQL = `
	SELECT t.course_id, count(l.id)
	FROM lessons l
	JOIN topics t ON t.id = l.topic_id
	WHERE t.tenant_id = $1 AND t.course_id = ANY($2)
	GROUP BY t.course_id`

// lessonCounts returns, for each course id given, how many lessons it holds.
func (r *PostgresRepository) lessonCounts(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, courseIDs []uuid.UUID) (map[uuid.UUID]int, error) {
	rows, err := tx.Query(ctx, lessonCountsByCourseSQL, tenantID, courseIDs)
	if err != nil {
		return nil, fmt.Errorf("catalog: count lessons: %w", err)
	}
	defer rows.Close()

	counts := make(map[uuid.UUID]int, len(courseIDs))
	for rows.Next() {
		var (
			id uuid.UUID
			n  int
		)
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("catalog: scan lesson count: %w", err)
		}
		counts[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: count lessons: %w", err)
	}
	return counts, nil
}

const createAnnouncementSQL = `
	INSERT INTO announcements (tenant_id, course_id, author_id, title, body)
	VALUES ($1, $2, $3, $4, $5)
	RETURNING id, title, body, created_at`

// CreateAnnouncement pins a notice to a course.
func (r *PostgresRepository) CreateAnnouncement(ctx context.Context, tx pgx.Tx, tenantID, courseID, authorID uuid.UUID, title, body string) (Announcement, error) {
	var a Announcement
	err := tx.QueryRow(ctx, createAnnouncementSQL, tenantID, courseID, authorID, title, body).
		Scan(&a.ID, &a.Title, &a.Body, &a.CreatedAt)
	if err != nil {
		return Announcement{}, fmt.Errorf("catalog: create announcement: %w", err)
	}
	return a, nil
}

const announcementsSQL = `
	SELECT id, title, body, created_at
	FROM announcements
	WHERE tenant_id = $1 AND course_id = $2
	ORDER BY created_at DESC, id`

// Announcements lists a course's notices, walking announcements_course_idx newest
// first.
func (r *PostgresRepository) Announcements(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) ([]Announcement, error) {
	rows, err := tx.Query(ctx, announcementsSQL, tenantID, courseID)
	if err != nil {
		return nil, fmt.Errorf("catalog: list announcements: %w", err)
	}
	defer rows.Close()

	announcements, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Announcement, error) {
		var a Announcement
		err := row.Scan(&a.ID, &a.Title, &a.Body, &a.CreatedAt)
		return a, err
	})
	if err != nil {
		return nil, fmt.Errorf("catalog: scan announcements: %w", err)
	}
	return announcements, nil
}

const deleteAnnouncementSQL = `DELETE FROM announcements WHERE tenant_id = $1 AND id = $2`

// DeleteAnnouncement removes a notice, reporting whether one was there.
func (r *PostgresRepository) DeleteAnnouncement(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (bool, error) {
	tag, err := tx.Exec(ctx, deleteAnnouncementSQL, tenantID, id)
	if err != nil {
		return false, fmt.Errorf("catalog: delete announcement: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// courseBySlugSQL hides unpublished courses unless the caller may see drafts.
//
// The filter is in the query, not in a check after the fact. A draft that is
// loaded and then discarded has already been loaded, and the first refactor that
// forgets the discard turns an unreleased course into a public one.
// The author's name is joined here rather than fetched after: a page that shows
// "created by" would otherwise cost a second query for a single string.
const courseBySlugSQL = `
	SELECT c.id, c.slug, c.title, c.summary, c.difficulty, c.status, c.published_at, c.drip_mode,
	       c.description, c.objectives, c.requirements, c.language,
	       c.preview_source, c.preview_url, c.preview_embed_url, c.image_key,
	       c.created_by, COALESCE(u.name, ''), c.created_at, c.updated_at
	FROM courses c
	LEFT JOIN users u ON u.id = c.created_by
	WHERE c.tenant_id = $1 AND lower(c.slug) = lower($2)
	  AND (c.status = 'published' OR $3)`

// CourseBySlug loads a single course. The unique index on (tenant_id, lower(slug))
// makes this an index lookup.
//
// includeDrafts must come from an authorisation decision, never from a request
// parameter.
func (r *PostgresRepository) CourseBySlug(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string, includeDrafts bool) (Course, error) {
	var c Course
	err := tx.QueryRow(ctx, courseBySlugSQL, tenantID, slug, includeDrafts).Scan(
		&c.ID, &c.Slug, &c.Title, &c.Summary, &c.Difficulty,
		&c.Status, &c.PublishedAt, &c.DripMode,
		&c.Description, &c.Objectives, &c.Requirements, &c.Language,
		&c.Preview.Source, &c.Preview.URL, &c.Preview.EmbedURL, &c.ImageKey,
		&c.CreatedBy, &c.InstructorName, &c.CreatedAt, &c.UpdatedAt)

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
	SELECT id, topic_id, title, content_type, duration_seconds, is_preview, position,
	       available_at, available_after_days
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
			&l.DurationSeconds, &l.IsPreview, &l.Position,
			&l.AvailableAt, &l.AvailableAfterDays)
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
