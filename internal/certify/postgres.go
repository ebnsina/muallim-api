package certify

import (
	"context"
	"errors"
	"fmt"
	"time"

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

const certificateColumns = `id, course_id, user_id, serial, learner_name, course_title,
	issued_at, template_id, title, body, signatory, revoked_at, revoked_reason`

func scanCertificate(row pgx.Row) (Certificate, error) {
	var c Certificate
	err := row.Scan(&c.ID, &c.CourseID, &c.UserID, &c.Serial, &c.LearnerName, &c.CourseTitle,
		&c.IssuedAt, &c.TemplateID, &c.Title, &c.Body, &c.Signatory, &c.RevokedAt, &c.RevokedReason)
	return c, err
}

/*
Subject reads the learner's name and the course's title as they stand now.

One query. Both are copied into the certificate rather than joined to it: a person
who changes their name has not invalidated what they earned under the old one, and
a course renamed next year did not rename what somebody finished this year.
*/
func (r *PostgresRepository) Subject(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) (Subject, error) {
	var s Subject
	err := tx.QueryRow(ctx,
		`SELECT u.name, c.title, c.certificate_template_id
		   FROM courses c, users u
		  WHERE c.tenant_id = $1 AND c.id = $2 AND u.id = $3`,
		tenantID, courseID, userID).Scan(&s.LearnerName, &s.CourseTitle, &s.TemplateID)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Subject{}, ErrNotFound
		}
		return Subject{}, fmt.Errorf("certify: read subject: %w", err)
	}
	return s, nil
}

/*
Issue writes the certificate, or leaves the one already there.

`ON CONFLICT DO NOTHING` on the learner-and-course index, then a read. Finishing a
course twice is finishing it once, and the serial of the first certificate is the
one the learner has written down.

The bool says whether this call wrote it, so the caller knows whether an audit line
is owed. A retried transaction should not record a second issuing of the same
certificate.
*/
func (r *PostgresRepository) Issue(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, c Certificate) (Certificate, bool, error) {
	written, err := scanCertificate(tx.QueryRow(ctx,
		`INSERT INTO certificates
		     (tenant_id, course_id, user_id, serial, learner_name, course_title, issued_at,
		      template_id, title, body, signatory)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		 ON CONFLICT (tenant_id, course_id, user_id) DO NOTHING
		 RETURNING `+certificateColumns,
		tenantID, c.CourseID, c.UserID, c.Serial, c.LearnerName, c.CourseTitle, c.IssuedAt,
		c.TemplateID, c.Title, c.Body, c.Signatory))

	if err == nil {
		return written, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		if isUniqueViolation(err) {
			// The serial collided. Eighty bits, so this is a bug in the source of
			// randomness rather than bad luck, and it is not something to retry into.
			return Certificate{}, false, fmt.Errorf("certify: serial collision: %w", err)
		}
		return Certificate{}, false, fmt.Errorf("certify: issue: %w", err)
	}

	// The conflict fired: the learner already has one. Return it.
	existing, err := scanCertificate(tx.QueryRow(ctx,
		`SELECT `+certificateColumns+`
		   FROM certificates WHERE tenant_id = $1 AND course_id = $2 AND user_id = $3`,
		tenantID, c.CourseID, c.UserID))
	if err != nil {
		return Certificate{}, false, fmt.Errorf("certify: read the certificate already issued: %w", err)
	}
	return existing, false, nil
}

func (r *PostgresRepository) BySerial(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, serial string) (Certificate, error) {
	certificate, err := scanCertificate(tx.QueryRow(ctx,
		`SELECT `+certificateColumns+` FROM certificates WHERE tenant_id = $1 AND serial = $2`,
		tenantID, serial))

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Certificate{}, ErrNotFound
		}
		return Certificate{}, fmt.Errorf("certify: verify: %w", err)
	}
	return certificate, nil
}

func (r *PostgresRepository) ForLearner(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, limit int) ([]Certificate, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+certificateColumns+`
		   FROM certificates
		  WHERE tenant_id = $1 AND user_id = $2
		  ORDER BY issued_at DESC, id DESC
		  LIMIT $3`,
		tenantID, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("certify: list a learner's certificates: %w", err)
	}
	defer rows.Close()

	var certificates []Certificate
	for rows.Next() {
		certificate, err := scanCertificate(rows)
		if err != nil {
			return nil, fmt.Errorf("certify: scan certificate: %w", err)
		}
		certificates = append(certificates, certificate)
	}
	return certificates, rows.Err()
}

// Revoke marks it, and never deletes it. Somebody has the code, and a code that
// stops resolving looks exactly like a code that was never real.
func (r *PostgresRepository) Revoke(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, serial, reason string) error {
	tag, err := tx.Exec(ctx,
		`UPDATE certificates SET revoked_at = now(), revoked_reason = $3
		  WHERE tenant_id = $1 AND serial = $2 AND revoked_at IS NULL`,
		tenantID, serial, reason)
	if err != nil {
		return fmt.Errorf("certify: revoke: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Either it does not exist, or it is revoked already. Revoking a revoked
		// certificate is not an error, but a serial nobody issued is.
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT true FROM certificates WHERE tenant_id = $1 AND serial = $2`,
			tenantID, serial).Scan(&exists); err != nil {
			return ErrNotFound
		}
	}
	return nil
}

// ------------------------------------------------------------------ templates

const templateColumns = `id, name, title, body, signatory`

func (r *PostgresRepository) TemplateByID(ctx context.Context, tx pgx.Tx, tenantID, templateID uuid.UUID) (Template, error) {
	var t Template
	err := tx.QueryRow(ctx,
		`SELECT `+templateColumns+` FROM certificate_templates WHERE tenant_id = $1 AND id = $2`,
		tenantID, templateID).Scan(&t.ID, &t.Name, &t.Title, &t.Body, &t.Signatory)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Template{}, ErrNotFound
		}
		return Template{}, fmt.Errorf("certify: load template: %w", err)
	}
	return t, nil
}

func (r *PostgresRepository) Templates(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]Template, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+templateColumns+` FROM certificate_templates
		  WHERE tenant_id = $1 ORDER BY lower(name), id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("certify: list templates: %w", err)
	}
	defer rows.Close()

	var templates []Template
	for rows.Next() {
		var t Template
		if err := rows.Scan(&t.ID, &t.Name, &t.Title, &t.Body, &t.Signatory); err != nil {
			return nil, fmt.Errorf("certify: scan template: %w", err)
		}
		templates = append(templates, t)
	}
	return templates, rows.Err()
}

func (r *PostgresRepository) CreateTemplate(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, t Template) (Template, error) {
	var created Template
	err := tx.QueryRow(ctx,
		`INSERT INTO certificate_templates (tenant_id, name, title, body, signatory)
		 VALUES ($1, $2, $3, $4, $5) RETURNING `+templateColumns,
		tenantID, t.Name, t.Title, t.Body, t.Signatory).
		Scan(&created.ID, &created.Name, &created.Title, &created.Body, &created.Signatory)

	if err != nil {
		if isUniqueViolation(err) {
			return Template{}, ErrTemplateExists
		}
		return Template{}, fmt.Errorf("certify: create template: %w", err)
	}
	return created, nil
}

func (r *PostgresRepository) DeleteTemplate(ctx context.Context, tx pgx.Tx, tenantID, templateID uuid.UUID) error {
	tag, err := tx.Exec(ctx,
		`DELETE FROM certificate_templates WHERE tenant_id = $1 AND id = $2`, tenantID, templateID)
	if err != nil {
		return fmt.Errorf("certify: delete template: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CourseTemplateID reads which template a course prints, or nil for the default.
func (r *PostgresRepository) CourseTemplateID(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string) (*uuid.UUID, error) {
	var id *uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT certificate_template_id FROM courses WHERE tenant_id = $1 AND slug = $2`,
		tenantID, slug).Scan(&id)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("certify: read course template: %w", err)
	}
	return id, nil
}

func (r *PostgresRepository) SetCourseTemplate(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string, templateID *uuid.UUID) error {
	tag, err := tx.Exec(ctx,
		`UPDATE courses SET certificate_template_id = $3, updated_at = now()
		  WHERE tenant_id = $1 AND slug = $2`,
		tenantID, slug, templateID)
	if err != nil {
		return fmt.Errorf("certify: set course template: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Issued is a page of what the workspace has awarded, newest first.
//
// Newest first, so the keyset runs backwards: `(issued_at, id) < ($2, $3)`. One
// comparison against the index that already satisfies the sort.
func (r *PostgresRepository) Issued(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *cursor, limit int) ([]Certificate, error) {
	var (
		afterTime *time.Time
		afterID   *uuid.UUID
	)
	if after != nil {
		afterTime, afterID = &after.IssuedAt, &after.ID
	}

	rows, err := tx.Query(ctx,
		`SELECT `+certificateColumns+`
		   FROM certificates
		  WHERE tenant_id = $1
		    AND ($2::timestamptz IS NULL OR (issued_at, id) < ($2, $3))
		  ORDER BY issued_at DESC, id DESC
		  LIMIT $4`,
		tenantID, afterTime, afterID, limit+1)
	if err != nil {
		return nil, fmt.Errorf("certify: list issued certificates: %w", err)
	}
	defer rows.Close()

	var certificates []Certificate
	for rows.Next() {
		certificate, err := scanCertificate(rows)
		if err != nil {
			return nil, fmt.Errorf("certify: scan certificate: %w", err)
		}
		certificates = append(certificates, certificate)
	}
	return certificates, rows.Err()
}
