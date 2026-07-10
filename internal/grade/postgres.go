package grade

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type PostgresRepository struct{}

func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

// 23505 is a unique violation. Matched on the interface rather than the concrete
// `*pgconn.PgError`, as everywhere else in this repository.
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

/*
UpsertItem records the assessment, resolving its course from its lesson.

One statement. The lesson knows its topic and the topic knows its course, so a
round trip to look the course up first would be a round trip inside a transaction
that is already holding a grading job open.

`ON CONFLICT` on `(tenant_id, source, source_id)`: a quiz that is edited updates
its item, and does not acquire a second one. The course is not updated on
conflict — a lesson does not move between courses, and if it ever does, the item
follows the lesson through its foreign key.
*/
func (r *PostgresRepository) UpsertItem(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, s Score) (uuid.UUID, error) {
	var itemID uuid.UUID

	err := tx.QueryRow(ctx,
		`INSERT INTO grade_items (tenant_id, course_id, lesson_id, source, source_id, title, max_points)
		 SELECT $1, t.course_id, l.id, $3, $4, $5, $6
		   FROM lessons l JOIN topics t ON t.id = l.topic_id
		  WHERE l.tenant_id = $1 AND l.id = $2
		 ON CONFLICT (tenant_id, source, source_id) DO UPDATE
		    SET title = excluded.title, max_points = excluded.max_points, updated_at = now()
		 RETURNING id`,
		tenantID, s.LessonID, s.Source, s.SourceID, s.Title, s.MaxPoints,
	).Scan(&itemID)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The SELECT matched nothing: the lesson is gone, or belongs to another
			// workspace. Either way there is no course to hang a grade on.
			return uuid.Nil, fmt.Errorf("%w: lesson %s", ErrNotFound, s.LessonID)
		}
		return uuid.Nil, fmt.Errorf("grade: record item: %w", err)
	}

	return itemID, nil
}

/*
UpsertEntry writes the learner's mark.

Idempotent: a retried grading job records the same score again rather than a
second one.

`max_points` is copied from the score, not read from the item, so raising an
assessment's worth afterwards does not rewrite the grades already given.

The `WHERE` on the update is what `KeepHighest` means. Cross-multiplied rather
than divided: integer division would call 1 of 3 and 1 of 4 equal, and floating
point would decide 0.1+0.2 cases by luck.
*/
func (r *PostgresRepository) UpsertEntry(ctx context.Context, tx pgx.Tx, tenantID, itemID uuid.UUID, s Score) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO grade_entries (tenant_id, grade_item_id, user_id, points, max_points)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (tenant_id, grade_item_id, user_id) DO UPDATE
		    SET points = excluded.points, max_points = excluded.max_points, graded_at = now()
		  WHERE NOT $6
		     OR excluded.points * grade_entries.max_points
		      > grade_entries.points * excluded.max_points`,
		tenantID, itemID, s.UserID, s.Points, s.MaxPoints, s.KeepHighest)

	if err != nil {
		return fmt.Errorf("grade: record entry: %w", err)
	}
	return nil
}

func (r *PostgresRepository) Items(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) ([]Item, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, course_id, lesson_id, source, source_id, title, max_points, created_at
		   FROM grade_items
		  WHERE tenant_id = $1 AND course_id = $2
		  ORDER BY created_at, id`,
		tenantID, courseID)
	if err != nil {
		return nil, fmt.Errorf("grade: list items: %w", err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		var item Item
		if err := rows.Scan(&item.ID, &item.CourseID, &item.LessonID, &item.Source,
			&item.SourceID, &item.Title, &item.MaxPoints, &item.CreatedAt); err != nil {
			return nil, fmt.Errorf("grade: scan item: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// EntriesForCourse is every mark in the course, for every learner, in one query.
// The caller stitches them to learners with a map. One query per learner is how a
// gradebook becomes unusable at the size of a real class.
func (r *PostgresRepository) EntriesForCourse(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) ([]Entry, error) {
	rows, err := tx.Query(ctx,
		`SELECT e.grade_item_id, e.user_id, e.points, e.max_points, e.graded_at
		   FROM grade_entries e
		   JOIN grade_items i ON i.id = e.grade_item_id
		  WHERE e.tenant_id = $1 AND i.tenant_id = $1 AND i.course_id = $2`,
		tenantID, courseID)
	if err != nil {
		return nil, fmt.Errorf("grade: list entries: %w", err)
	}
	defer rows.Close()

	return scanEntries(rows)
}

func (r *PostgresRepository) EntriesForLearner(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) ([]Entry, error) {
	rows, err := tx.Query(ctx,
		`SELECT e.grade_item_id, e.user_id, e.points, e.max_points, e.graded_at
		   FROM grade_entries e
		   JOIN grade_items i ON i.id = e.grade_item_id
		  WHERE e.tenant_id = $1 AND i.tenant_id = $1 AND i.course_id = $2 AND e.user_id = $3`,
		tenantID, courseID, userID)
	if err != nil {
		return nil, fmt.Errorf("grade: list a learner's entries: %w", err)
	}
	defer rows.Close()

	return scanEntries(rows)
}

func scanEntries(rows pgx.Rows) ([]Entry, error) {
	var entries []Entry
	for rows.Next() {
		var entry Entry
		if err := rows.Scan(&entry.ItemID, &entry.UserID, &entry.Points,
			&entry.MaxPoints, &entry.GradedAt); err != nil {
			return nil, fmt.Errorf("grade: scan entry: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// Learners is everybody enrolled, whether or not they have been graded. A
// gradebook that listed only the learners with marks would hide the ones who have
// handed in nothing, who are the ones a teacher is looking for.
func (r *PostgresRepository) Learners(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) ([]Learner, error) {
	rows, err := tx.Query(ctx,
		`SELECT u.id, u.name, u.email
		   FROM enrolments e JOIN users u ON u.id = e.user_id
		  WHERE e.tenant_id = $1 AND e.course_id = $2 AND e.status IN ('active', 'completed')
		  ORDER BY u.name, u.id`,
		tenantID, courseID)
	if err != nil {
		return nil, fmt.Errorf("grade: list learners: %w", err)
	}
	defer rows.Close()

	var learners []Learner
	for rows.Next() {
		var learner Learner
		if err := rows.Scan(&learner.UserID, &learner.Name, &learner.Email); err != nil {
			return nil, fmt.Errorf("grade: scan learner: %w", err)
		}
		learners = append(learners, learner)
	}
	return learners, rows.Err()
}

func (r *PostgresRepository) CourseBySlug(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string) (Course, error) {
	var course Course
	err := tx.QueryRow(ctx,
		`SELECT id, slug, title, grading_scale_id FROM courses WHERE tenant_id = $1 AND slug = $2`,
		tenantID, slug).Scan(&course.ID, &course.Slug, &course.Title, &course.ScaleID)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Course{}, ErrNotFound
		}
		return Course{}, fmt.Errorf("grade: load course: %w", err)
	}
	return course, nil
}

// ScaleByID loads a scale and its bands. Two queries, and not one per band.
func (r *PostgresRepository) ScaleByID(ctx context.Context, tx pgx.Tx, tenantID, scaleID uuid.UUID) (Scale, error) {
	var scale Scale
	err := tx.QueryRow(ctx,
		`SELECT id, name FROM grading_scales WHERE tenant_id = $1 AND id = $2`,
		tenantID, scaleID).Scan(&scale.ID, &scale.Name)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Scale{}, ErrNotFound
		}
		return Scale{}, fmt.Errorf("grade: load scale: %w", err)
	}

	scale.Bands, err = bandsOf(ctx, tx, tenantID, []uuid.UUID{scaleID})
	if err != nil {
		return Scale{}, err
	}
	return scale, nil
}

// Scales lists the workspace's scales with their bands, in two queries. Reading
// the bands inside a loop over the scales is the N+1 this batches away.
func (r *PostgresRepository) Scales(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]Scale, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, name FROM grading_scales WHERE tenant_id = $1 ORDER BY lower(name), id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("grade: list scales: %w", err)
	}
	defer rows.Close()

	var scales []Scale
	var ids []uuid.UUID
	for rows.Next() {
		var scale Scale
		if err := rows.Scan(&scale.ID, &scale.Name); err != nil {
			return nil, fmt.Errorf("grade: scan scale: %w", err)
		}
		scales = append(scales, scale)
		ids = append(ids, scale.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(scales) == 0 {
		return nil, nil
	}

	bands, err := bandsOf(ctx, tx, tenantID, ids)
	if err != nil {
		return nil, err
	}

	byScale := make(map[uuid.UUID][]Band, len(scales))
	for _, band := range bands {
		byScale[band.ScaleID] = append(byScale[band.ScaleID], band)
	}
	for i := range scales {
		scales[i].Bands = byScale[scales[i].ID]
	}

	return scales, nil
}

// bandsOf reads the bands of any number of scales with one `= ANY($2)`.
func bandsOf(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, scaleIDs []uuid.UUID) ([]Band, error) {
	rows, err := tx.Query(ctx,
		`SELECT scale_id, label, min_percent, is_pass
		   FROM grading_bands
		  WHERE tenant_id = $1 AND scale_id = ANY($2)
		  ORDER BY scale_id, min_percent DESC`,
		tenantID, scaleIDs)
	if err != nil {
		return nil, fmt.Errorf("grade: list bands: %w", err)
	}
	defer rows.Close()

	var bands []Band
	for rows.Next() {
		var band Band
		if err := rows.Scan(&band.ScaleID, &band.Label, &band.Min, &band.IsPass); err != nil {
			return nil, fmt.Errorf("grade: scan band: %w", err)
		}
		bands = append(bands, band)
	}
	return bands, rows.Err()
}

// CreateScale writes the scale and its bands. Two statements, and the bands go in
// one `unnest` rather than one INSERT per band.
func (r *PostgresRepository) CreateScale(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, s Scale) (Scale, error) {
	var created Scale
	err := tx.QueryRow(ctx,
		`INSERT INTO grading_scales (tenant_id, name) VALUES ($1, $2) RETURNING id, name`,
		tenantID, s.Name).Scan(&created.ID, &created.Name)

	if err != nil {
		if isUniqueViolation(err) {
			return Scale{}, ErrScaleExists
		}
		return Scale{}, fmt.Errorf("grade: create scale: %w", err)
	}

	labels := make([]string, len(s.Bands))
	floors := make([]int32, len(s.Bands))
	passes := make([]bool, len(s.Bands))
	for i, band := range s.Bands {
		labels[i], floors[i], passes[i] = band.Label, int32(band.Min), band.IsPass
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO grading_bands (tenant_id, scale_id, label, min_percent, is_pass)
		 SELECT $1, $2, b.label, b.min_percent, b.is_pass
		   FROM unnest($3::text[], $4::int[], $5::bool[]) AS b(label, min_percent, is_pass)`,
		tenantID, created.ID, labels, floors, passes)
	if err != nil {
		return Scale{}, fmt.Errorf("grade: create bands: %w", err)
	}

	created.Bands = s.Bands
	return created, nil
}

func (r *PostgresRepository) DeleteScale(ctx context.Context, tx pgx.Tx, tenantID, scaleID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `DELETE FROM grading_scales WHERE tenant_id = $1 AND id = $2`, tenantID, scaleID)
	if err != nil {
		return fmt.Errorf("grade: delete scale: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *PostgresRepository) SetCourseScale(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, scaleID *uuid.UUID) error {
	tag, err := tx.Exec(ctx,
		`UPDATE courses SET grading_scale_id = $3, updated_at = now() WHERE tenant_id = $1 AND id = $2`,
		tenantID, courseID, scaleID)
	if err != nil {
		return fmt.Errorf("grade: set course scale: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
