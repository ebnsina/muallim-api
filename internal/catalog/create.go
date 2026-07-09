package catalog

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// slugPattern permits lowercase letters, digits, and interior hyphens. Slugs
// appear in URLs, so anything else is either an encoding problem or an attempt
// at one.
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// NewCourse describes a course to create.
type NewCourse struct {
	Slug       string
	Title      string
	Summary    string
	Difficulty string
}

// Validate checks the input before any database work.
func (n NewCourse) Validate() error {
	if !slugPattern.MatchString(n.Slug) {
		return fmt.Errorf("%w: %q", ErrInvalidSlug, n.Slug)
	}
	return nil
}

// AuditEntry mirrors audit.Entry. Restated here because a domain package may not
// import a sibling; cmd/ wires an adapter that satisfies AuditRecorder.
type AuditEntry struct {
	ActorID    *uuid.UUID
	Action     string
	TargetType string
	TargetID   string
	IP         netip.Addr
	UserAgent  string
	Metadata   map[string]any
}

// AuditRecorder appends to the audit trail inside the caller's transaction.
type AuditRecorder interface {
	Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e AuditEntry) error
}

// Author identifies who is creating a course, for the audit trail.
type Author struct {
	UserID    uuid.UUID
	IP        netip.Addr
	UserAgent string
}

// CreateCourse inserts a draft course.
//
// Authorisation is the caller's responsibility and is performed in the transport
// layer against auth.Service, which is the only component that knows what a
// permission is. This service enforces the invariants of a course, not of a
// person.
//
// A new course is always a draft. Publishing is a separate, separately
// authorised act: `course:write` lets an instructor draft, `course:publish` lets
// them make it visible to students, and conflating the two means every author can
// publish.
func (s *Service) CreateCourse(ctx context.Context, tenantID uuid.UUID, n NewCourse, author Author) (Course, error) {
	if err := n.Validate(); err != nil {
		return Course{}, err
	}
	if n.Difficulty == "" {
		n.Difficulty = "beginner"
	}

	var created Course
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		course, err := s.repo.CreateCourse(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		created = course

		// The audit entry commits with the course, or neither does. An audit record
		// that commits separately from the thing it describes will eventually
		// disagree with it.
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionCourseCreated,
			TargetType: "course", TargetID: course.ID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
			Metadata: map[string]any{"slug": course.Slug, "title": course.Title},
		})
	})
	if err != nil {
		return Course{}, err
	}
	return created, nil
}

const createCourseSQL = `
	INSERT INTO courses (tenant_id, slug, title, summary, difficulty, status)
	VALUES ($1, $2, $3, $4, $5, 'draft')
	RETURNING id, slug, title, summary, difficulty, status, published_at, drip_mode, created_at, updated_at`

// CreateCourse inserts a course and returns it.
func (r *PostgresRepository) CreateCourse(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewCourse) (Course, error) {
	var c Course
	err := tx.QueryRow(ctx, createCourseSQL, tenantID, n.Slug, n.Title, n.Summary, n.Difficulty).Scan(
		&c.ID, &c.Slug, &c.Title, &c.Summary, &c.Difficulty,
		&c.Status, &c.PublishedAt, &c.DripMode, &c.CreatedAt, &c.UpdatedAt)

	if err != nil {
		if isUniqueViolation(err) {
			return Course{}, fmt.Errorf("%w: %q", ErrSlugTaken, n.Slug)
		}
		return Course{}, fmt.Errorf("catalog: create course: %w", err)
	}
	return c, nil
}

// isUniqueViolation reports whether err is a Postgres unique index conflict.
//
// Checked rather than pre-queried: a "does this slug exist" SELECT before the
// INSERT is a race, and the unique index has to be there regardless.
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}
