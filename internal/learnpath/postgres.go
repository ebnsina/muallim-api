package learnpath

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

const pathColumns = `id, slug, title, description, status, created_at, updated_at`

func (r *PostgresRepository) Create(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewPath) (Path, error) {
	rows, err := tx.Query(ctx,
		`INSERT INTO learning_paths (tenant_id, slug, title, description)
		 VALUES ($1, $2, $3, $4)
		 RETURNING `+pathColumns,
		tenantID, n.Slug, n.Title, n.Description)
	if err == nil {
		var p Path
		p, err = pgx.CollectExactlyOneRow(rows, scanPath)
		if err == nil {
			return p, nil
		}
	}
	if isUniqueViolation(err) {
		return Path{}, fmt.Errorf("%w: %q", ErrDuplicate, n.Slug)
	}
	return Path{}, fmt.Errorf("learnpath: create path: %w", err)
}

// Two statements, not one with an `OR $x IS NULL` predicate on status: each shape
// gets the index that covers it, and neither leaves a Sort node on the path.
const listAllSQL = `
	SELECT ` + pathColumns + ` FROM learning_paths
	WHERE tenant_id = $1
	  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4::uuid))
	ORDER BY created_at DESC, id DESC LIMIT $2`

const listByStatusSQL = `
	SELECT ` + pathColumns + ` FROM learning_paths
	WHERE tenant_id = $1 AND status = $5
	  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4::uuid))
	ORDER BY created_at DESC, id DESC LIMIT $2`

func (r *PostgresRepository) List(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f PathFilter, after *cursor, limit int) ([]Path, error) {
	var afterTime, afterID any
	if after != nil {
		afterTime, afterID = after.CreatedAt, after.ID
	}

	var rows pgx.Rows
	var err error
	if f.Status != "" {
		rows, err = tx.Query(ctx, listByStatusSQL, tenantID, limit, afterTime, afterID, f.Status)
	} else {
		rows, err = tx.Query(ctx, listAllSQL, tenantID, limit, afterTime, afterID)
	}
	if err != nil {
		return nil, fmt.Errorf("learnpath: list: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanPath)
}

func (r *PostgresRepository) BySlug(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string) (Path, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+pathColumns+` FROM learning_paths WHERE tenant_id = $1 AND slug = $2`,
		tenantID, slug)
	if err != nil {
		return Path{}, fmt.Errorf("learnpath: load path: %w", err)
	}
	p, err := pgx.CollectExactlyOneRow(rows, scanPath)
	if errors.Is(err, pgx.ErrNoRows) {
		return Path{}, ErrNotFound
	}
	if err != nil {
		return Path{}, fmt.Errorf("learnpath: load path: %w", err)
	}
	return p, nil
}

func (r *PostgresRepository) Update(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string, p PathPatch) (Path, error) {
	rows, err := tx.Query(ctx,
		`UPDATE learning_paths SET
		     title       = coalesce($3, title),
		     description = coalesce($4, description),
		     status      = coalesce($5, status),
		     updated_at  = now()
		 WHERE tenant_id = $1 AND slug = $2
		 RETURNING `+pathColumns,
		tenantID, slug, p.Title, p.Description, p.Status)
	if err != nil {
		return Path{}, fmt.Errorf("learnpath: update path: %w", err)
	}
	updated, err := pgx.CollectExactlyOneRow(rows, scanPath)
	if errors.Is(err, pgx.ErrNoRows) {
		return Path{}, ErrNotFound
	}
	if err != nil {
		return Path{}, fmt.Errorf("learnpath: update path: %w", err)
	}
	return updated, nil
}

func (r *PostgresRepository) Delete(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM learning_paths WHERE tenant_id = $1 AND slug = $2`, tenantID, slug)
	if err != nil {
		return fmt.Errorf("learnpath: delete path: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CourseRefs loads a path's courses in order, in one query.
func (r *PostgresRepository) CourseRefs(ctx context.Context, tx pgx.Tx, tenantID, pathID uuid.UUID) ([]CourseRef, error) {
	rows, err := tx.Query(ctx,
		`SELECT course_id, position FROM learning_path_courses
		 WHERE tenant_id = $1 AND path_id = $2
		 ORDER BY position`,
		tenantID, pathID)
	if err != nil {
		return nil, fmt.Errorf("learnpath: course refs: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (CourseRef, error) {
		var c CourseRef
		err := row.Scan(&c.CourseID, &c.Position)
		return c, err
	})
}

const insertCoursesSQL = `
	INSERT INTO learning_path_courses (tenant_id, path_id, course_id, position)
	SELECT $1, $2, v.id, v.rank - 1
	FROM unnest($3::uuid[]) WITH ORDINALITY AS v(id, rank)`

// SetCourses replaces a path's whole membership. The clear and the ordinality
// insert run in the caller's transaction: the old rows are gone before the new
// ones are checked, so the non-deferrable course-id unique never trips on a
// course that stays. A course id that names no course is a foreign-key violation,
// reported as ErrInvalid rather than a 500.
func (r *PostgresRepository) SetCourses(ctx context.Context, tx pgx.Tx, tenantID, pathID uuid.UUID, order []uuid.UUID) error {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT true FROM learning_paths WHERE tenant_id = $1 AND id = $2`, tenantID, pathID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("learnpath: check path: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM learning_path_courses WHERE tenant_id = $1 AND path_id = $2`,
		tenantID, pathID); err != nil {
		return fmt.Errorf("learnpath: clear courses: %w", err)
	}

	if len(order) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, insertCoursesSQL, tenantID, pathID, order); err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("%w: a course does not exist", ErrInvalid)
		}
		return fmt.Errorf("learnpath: set courses: %w", err)
	}
	return nil
}

func scanPath(row pgx.CollectableRow) (Path, error) {
	var p Path
	err := row.Scan(&p.ID, &p.Slug, &p.Title, &p.Description, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}
