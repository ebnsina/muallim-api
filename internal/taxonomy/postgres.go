package taxonomy

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PostgresRepository is the Postgres-backed Repository.
type PostgresRepository struct{}

// NewPostgresRepository returns a PostgresRepository.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23503"
}

const categoryColumns = `id, name, slug, created_at, updated_at`

func (r *PostgresRepository) CreateCategory(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewTerm) (Category, error) {
	var c Category
	err := tx.QueryRow(ctx,
		`INSERT INTO course_categories (tenant_id, name, slug)
		 VALUES ($1, $2, $3)
		 RETURNING `+categoryColumns,
		tenantID, n.Name, n.Slug).
		Scan(&c.ID, &c.Name, &c.Slug, &c.CreatedAt, &c.UpdatedAt)
	if isUniqueViolation(err) {
		return Category{}, ErrDuplicate
	}
	if err != nil {
		return Category{}, fmt.Errorf("taxonomy: create category: %w", err)
	}
	return c, nil
}

const categoriesSQL = `
	SELECT ` + categoryColumns + ` FROM course_categories
	WHERE tenant_id = $1
	  AND ($3::text IS NULL OR (name, id) > ($3, $4::uuid))
	ORDER BY name, id LIMIT $2`

func (r *PostgresRepository) Categories(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *cursor, limit int) ([]Category, error) {
	var afterName, afterID any
	if after != nil {
		afterName, afterID = after.Name, after.ID
	}
	rows, err := tx.Query(ctx, categoriesSQL, tenantID, limit, afterName, afterID)
	if err != nil {
		return nil, fmt.Errorf("taxonomy: categories: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanCategory)
}

func (r *PostgresRepository) DeleteCategory(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error {
	tag, err := tx.Exec(ctx,
		`DELETE FROM course_categories WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("taxonomy: delete category: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

const tagColumns = `id, name, slug, created_at, updated_at`

func (r *PostgresRepository) CreateTag(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewTerm) (Tag, error) {
	var t Tag
	err := tx.QueryRow(ctx,
		`INSERT INTO course_tags (tenant_id, name, slug)
		 VALUES ($1, $2, $3)
		 RETURNING `+tagColumns,
		tenantID, n.Name, n.Slug).
		Scan(&t.ID, &t.Name, &t.Slug, &t.CreatedAt, &t.UpdatedAt)
	if isUniqueViolation(err) {
		return Tag{}, ErrDuplicate
	}
	if err != nil {
		return Tag{}, fmt.Errorf("taxonomy: create tag: %w", err)
	}
	return t, nil
}

const tagsSQL = `
	SELECT ` + tagColumns + ` FROM course_tags
	WHERE tenant_id = $1
	  AND ($3::text IS NULL OR (name, id) > ($3, $4::uuid))
	ORDER BY name, id LIMIT $2`

func (r *PostgresRepository) Tags(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *cursor, limit int) ([]Tag, error) {
	var afterName, afterID any
	if after != nil {
		afterName, afterID = after.Name, after.ID
	}
	rows, err := tx.Query(ctx, tagsSQL, tenantID, limit, afterName, afterID)
	if err != nil {
		return nil, fmt.Errorf("taxonomy: tags: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanTag)
}

func (r *PostgresRepository) DeleteTag(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error {
	tag, err := tx.Exec(ctx,
		`DELETE FROM course_tags WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("taxonomy: delete tag: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetCourseCategory files a course under a category, replacing any existing one,
// or clears it when categoryID is nil. The unique (tenant_id, course_id) makes the
// upsert a replace. An unknown course or category surfaces as a FK violation.
func (r *PostgresRepository) SetCourseCategory(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, categoryID *uuid.UUID) error {
	if categoryID == nil {
		_, err := tx.Exec(ctx,
			`DELETE FROM course_category_links WHERE tenant_id = $1 AND course_id = $2`,
			tenantID, courseID)
		if err != nil {
			return fmt.Errorf("taxonomy: clear course category: %w", err)
		}
		return nil
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO course_category_links (tenant_id, course_id, category_id)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (tenant_id, course_id) DO UPDATE SET category_id = EXCLUDED.category_id`,
		tenantID, courseID, *categoryID)
	if isForeignKeyViolation(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("taxonomy: set course category: %w", err)
	}
	return nil
}

func (r *PostgresRepository) CategoryForCourse(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) (*Category, error) {
	rows, err := tx.Query(ctx,
		`SELECT c.id, c.name, c.slug, c.created_at, c.updated_at
		 FROM course_category_links l
		 JOIN course_categories c ON c.tenant_id = l.tenant_id AND c.id = l.category_id
		 WHERE l.tenant_id = $1 AND l.course_id = $2`,
		tenantID, courseID)
	if err != nil {
		return nil, fmt.Errorf("taxonomy: category for course: %w", err)
	}
	c, err := pgx.CollectExactlyOneRow(rows, scanCategory)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("taxonomy: category for course: %w", err)
	}
	return &c, nil
}

// SetCourseTags replaces a course's tags with exactly tagIDs. One statement: a CTE
// deletes the tags no longer wanted and the insert adds the new ones, so a
// concurrent reader never sees the course with no tags mid-update. An empty tagIDs
// clears them. An unknown tag surfaces as a FK violation.
func (r *PostgresRepository) SetCourseTags(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, tagIDs []uuid.UUID) error {
	_, err := tx.Exec(ctx,
		`WITH del AS (
		     DELETE FROM course_tag_links
		     WHERE tenant_id = $1 AND course_id = $2 AND tag_id <> ALL($3::uuid[])
		 )
		 INSERT INTO course_tag_links (tenant_id, course_id, tag_id)
		 SELECT $1, $2, t FROM unnest($3::uuid[]) AS t
		 ON CONFLICT (tenant_id, course_id, tag_id) DO NOTHING`,
		tenantID, courseID, tagIDs)
	if isForeignKeyViolation(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("taxonomy: set course tags: %w", err)
	}
	return nil
}

func (r *PostgresRepository) TagsForCourse(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) ([]Tag, error) {
	rows, err := tx.Query(ctx,
		`SELECT t.id, t.name, t.slug, t.created_at, t.updated_at
		 FROM course_tag_links l
		 JOIN course_tags t ON t.tenant_id = l.tenant_id AND t.id = l.tag_id
		 WHERE l.tenant_id = $1 AND l.course_id = $2
		 ORDER BY t.name, t.id`,
		tenantID, courseID)
	if err != nil {
		return nil, fmt.Errorf("taxonomy: tags for course: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanTag)
}

func (r *PostgresRepository) CourseIDsInCategory(ctx context.Context, tx pgx.Tx, tenantID, categoryID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx,
		`SELECT course_id FROM course_category_links
		 WHERE tenant_id = $1 AND category_id = $2 ORDER BY course_id`,
		tenantID, categoryID)
	if err != nil {
		return nil, fmt.Errorf("taxonomy: course ids in category: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

func (r *PostgresRepository) CourseIDsWithTag(ctx context.Context, tx pgx.Tx, tenantID, tagID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx,
		`SELECT course_id FROM course_tag_links
		 WHERE tenant_id = $1 AND tag_id = $2 ORDER BY course_id`,
		tenantID, tagID)
	if err != nil {
		return nil, fmt.Errorf("taxonomy: course ids with tag: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

func (r *PostgresRepository) CourseIDBySlug(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT id FROM courses WHERE tenant_id = $1 AND lower(slug) = lower($2)`,
		tenantID, slug).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("taxonomy: course by slug: %w", err)
	}
	return id, nil
}

func scanCategory(row pgx.CollectableRow) (Category, error) {
	var c Category
	err := row.Scan(&c.ID, &c.Name, &c.Slug, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func scanTag(row pgx.CollectableRow) (Tag, error) {
	var t Tag
	err := row.Scan(&t.ID, &t.Name, &t.Slug, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}
