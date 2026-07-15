package notices

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgresRepository is the Postgres-backed Repository.
type PostgresRepository struct{}

// NewPostgresRepository returns a PostgresRepository.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

const columns = `id, title, body, audience, target_id, channel, recipient_count, created_at`

func (r *PostgresRepository) Create(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewNotice, channel string, postedBy uuid.UUID) (Notice, error) {
	var notice Notice
	err := tx.QueryRow(ctx,
		`INSERT INTO notices (tenant_id, title, body, audience, target_id, channel, posted_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING `+columns,
		tenantID, n.Title, n.Body, n.Audience, n.TargetID, channel, postedBy).
		Scan(&notice.ID, &notice.Title, &notice.Body, &notice.Audience, &notice.TargetID,
			&notice.Channel, &notice.RecipientCount, &notice.CreatedAt)
	if err != nil {
		return Notice{}, fmt.Errorf("notices: create: %w", err)
	}
	return notice, nil
}

// RecipientsFor resolves an audience to the distinct guardians who have an email.
// Each audience is its own statement so the guardian/student join is only paid for
// when the audience actually narrows to a class or section.
func (r *PostgresRepository) RecipientsFor(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, audience string, target *uuid.UUID) ([]Recipient, error) {
	var (
		rows pgx.Rows
		err  error
	)
	switch audience {
	case AudienceAll:
		rows, err = tx.Query(ctx,
			`SELECT DISTINCT g.full_name, g.email FROM guardians g
			 WHERE g.tenant_id = $1 AND g.email <> ''`, tenantID)
	case AudienceClass:
		rows, err = tx.Query(ctx,
			`SELECT DISTINCT g.full_name, g.email
			 FROM guardians g
			 JOIN student_guardians sg ON sg.guardian_id = g.id AND sg.tenant_id = g.tenant_id
			 JOIN students st ON st.id = sg.student_id AND st.tenant_id = sg.tenant_id
			 WHERE g.tenant_id = $1 AND st.grade_level_id = $2 AND g.email <> ''`, tenantID, target)
	case AudienceSection:
		rows, err = tx.Query(ctx,
			`SELECT DISTINCT g.full_name, g.email
			 FROM guardians g
			 JOIN student_guardians sg ON sg.guardian_id = g.id AND sg.tenant_id = g.tenant_id
			 JOIN students st ON st.id = sg.student_id AND st.tenant_id = sg.tenant_id
			 WHERE g.tenant_id = $1 AND st.section_id = $2 AND g.email <> ''`, tenantID, target)
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidNotice, audience)
	}
	if err != nil {
		return nil, fmt.Errorf("notices: recipients: %w", err)
	}
	defer rows.Close()

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Recipient, error) {
		var rec Recipient
		err := row.Scan(&rec.Name, &rec.Email)
		return rec, err
	})
}

func (r *PostgresRepository) SetRecipientCount(ctx context.Context, tx pgx.Tx, tenantID, noticeID uuid.UUID, count int) error {
	_, err := tx.Exec(ctx,
		`UPDATE notices SET recipient_count = $3 WHERE tenant_id = $1 AND id = $2`,
		tenantID, noticeID, count)
	if err != nil {
		return fmt.Errorf("notices: set recipient count: %w", err)
	}
	return nil
}

func (r *PostgresRepository) Notices(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *cursor, limit int) ([]Notice, error) {
	var afterTime, afterID any
	if after != nil {
		afterTime, afterID = after.CreatedAt, after.ID
	}
	rows, err := tx.Query(ctx,
		`SELECT `+columns+` FROM notices
		 WHERE tenant_id = $1
		   AND ($2::timestamptz IS NULL OR (created_at, id) < ($2, $3::uuid))
		 ORDER BY created_at DESC, id DESC LIMIT $4`,
		tenantID, afterTime, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("notices: list: %w", err)
	}
	defer rows.Close()

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Notice, error) {
		var n Notice
		err := row.Scan(&n.ID, &n.Title, &n.Body, &n.Audience, &n.TargetID, &n.Channel, &n.RecipientCount, &n.CreatedAt)
		return n, err
	})
}
