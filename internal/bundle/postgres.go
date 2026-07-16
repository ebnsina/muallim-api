package bundle

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

const bundleColumns = `id, slug, name, description, price_amount, currency, created_at, updated_at`

func (r *PostgresRepository) Create(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewBundle) (Bundle, error) {
	rows, err := tx.Query(ctx,
		`INSERT INTO bundles (tenant_id, slug, name, description, price_amount, currency)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING `+bundleColumns,
		tenantID, n.Slug, n.Name, n.Description, n.PriceAmount, n.Currency)
	if err != nil {
		return Bundle{}, fmt.Errorf("bundle: create: %w", err)
	}
	b, err := pgx.CollectExactlyOneRow(rows, scanBundle)
	if isUniqueViolation(err) {
		return Bundle{}, fmt.Errorf("%w: %q", ErrDuplicate, n.Slug)
	}
	if err != nil {
		return Bundle{}, fmt.Errorf("bundle: create: %w", err)
	}
	return b, nil
}

const listSQL = `
	SELECT ` + bundleColumns + ` FROM bundles
	WHERE tenant_id = $1
	  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4::uuid))
	ORDER BY created_at DESC, id DESC LIMIT $2`

func (r *PostgresRepository) List(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *cursor, limit int) ([]Bundle, error) {
	var afterTime, afterID any
	if after != nil {
		afterTime, afterID = after.CreatedAt, after.ID
	}
	rows, err := tx.Query(ctx, listSQL, tenantID, limit, afterTime, afterID)
	if err != nil {
		return nil, fmt.Errorf("bundle: list: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanBundle)
}

func (r *PostgresRepository) BySlug(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string) (Bundle, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+bundleColumns+` FROM bundles WHERE tenant_id = $1 AND lower(slug) = lower($2)`,
		tenantID, slug)
	if err != nil {
		return Bundle{}, fmt.Errorf("bundle: by slug: %w", err)
	}
	b, err := pgx.CollectExactlyOneRow(rows, scanBundle)
	if errors.Is(err, pgx.ErrNoRows) {
		return Bundle{}, ErrNotFound
	}
	return b, err
}

func (r *PostgresRepository) ByID(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Bundle, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+bundleColumns+` FROM bundles WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return Bundle{}, fmt.Errorf("bundle: by id: %w", err)
	}
	b, err := pgx.CollectExactlyOneRow(rows, scanBundle)
	if errors.Is(err, pgx.ErrNoRows) {
		return Bundle{}, ErrNotFound
	}
	return b, err
}

// Update rewrites the fields a BundlePatch names; COALESCE leaves a nil field alone.
func (r *PostgresRepository) Update(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string, p BundlePatch) (Bundle, error) {
	rows, err := tx.Query(ctx,
		`UPDATE bundles SET
		     name = COALESCE($3, name),
		     description = COALESCE($4, description),
		     price_amount = COALESCE($5, price_amount),
		     updated_at = now()
		 WHERE tenant_id = $1 AND lower(slug) = lower($2)
		 RETURNING `+bundleColumns,
		tenantID, slug, p.Name, p.Description, p.PriceAmount)
	if err != nil {
		return Bundle{}, fmt.Errorf("bundle: update: %w", err)
	}
	b, err := pgx.CollectExactlyOneRow(rows, scanBundle)
	if errors.Is(err, pgx.ErrNoRows) {
		return Bundle{}, ErrNotFound
	}
	return b, err
}

func (r *PostgresRepository) Delete(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM bundles WHERE tenant_id = $1 AND lower(slug) = lower($2)`, tenantID, slug)
	if err != nil {
		return fmt.Errorf("bundle: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetCourses replaces the bundle's courses with the submitted order. Delete-then-insert
// in one transaction; `unnest(...) WITH ORDINALITY` gives each course a dense position
// from its index in the list, so the whole reorder is one insert.
func (r *PostgresRepository) SetCourses(ctx context.Context, tx pgx.Tx, tenantID, bundleID uuid.UUID, courseIDs []uuid.UUID) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM bundle_courses WHERE tenant_id = $1 AND bundle_id = $2`, tenantID, bundleID); err != nil {
		return fmt.Errorf("bundle: clear courses: %w", err)
	}
	if len(courseIDs) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO bundle_courses (tenant_id, bundle_id, course_id, position)
		 SELECT $1, $2, cid, ord - 1
		 FROM unnest($3::uuid[]) WITH ORDINALITY AS t(cid, ord)`,
		tenantID, bundleID, courseIDs)
	if err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("%w: a course in the list does not exist", ErrInvalid)
		}
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: a course is named twice", ErrInvalid)
		}
		return fmt.Errorf("bundle: set courses: %w", err)
	}
	return nil
}

// CoursesFor loads the ordered courses of every named bundle in one query, batched
// with `= ANY`, and stitches them into a map keyed by bundle id — no query per bundle.
func (r *PostgresRepository) CoursesFor(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, bundleIDs []uuid.UUID) (map[uuid.UUID][]CourseRef, error) {
	out := make(map[uuid.UUID][]CourseRef, len(bundleIDs))
	if len(bundleIDs) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx,
		`SELECT bundle_id, course_id, position FROM bundle_courses
		 WHERE tenant_id = $1 AND bundle_id = ANY($2)
		 ORDER BY bundle_id, position, id`,
		tenantID, bundleIDs)
	if err != nil {
		return nil, fmt.Errorf("bundle: courses for: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var bundleID uuid.UUID
		var ref CourseRef
		if err := rows.Scan(&bundleID, &ref.CourseID, &ref.Position); err != nil {
			return nil, fmt.Errorf("bundle: scan course ref: %w", err)
		}
		out[bundleID] = append(out[bundleID], ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("bundle: courses for: %w", err)
	}
	return out, nil
}

func scanBundle(row pgx.CollectableRow) (Bundle, error) {
	var b Bundle
	err := row.Scan(&b.ID, &b.Slug, &b.Name, &b.Description, &b.PriceAmount, &b.Currency, &b.CreatedAt, &b.UpdatedAt)
	return b, err
}
