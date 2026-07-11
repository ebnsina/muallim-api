package notify

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgresRepository is the notifications table.
type PostgresRepository struct{}

// NewPostgresRepository returns one.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

const insertSQL = `
	INSERT INTO notifications (tenant_id, user_id, kind, title, body, link)
	VALUES ($1, $2, $3, $4, $5, $6)`

// Insert writes one notification.
func (r *PostgresRepository) Insert(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n Notification) error {
	_, err := tx.Exec(ctx, insertSQL, tenantID, n.UserID, n.Kind, n.Title, n.Body, n.Link)
	if err != nil {
		return fmt.Errorf("notify: insert: %w", err)
	}
	return nil
}

// listSQL is a keyset page over (created_at, id) descending, scoped to one
// recipient. The row comparison is a single seek on notifications_recipient_idx.
const listSQL = `
	SELECT id, user_id, kind, title, body, link, read_at, created_at
	FROM notifications
	WHERE tenant_id = $1 AND user_id = $2
	  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4))
	ORDER BY created_at DESC, id DESC
	LIMIT $5`

// List returns one keyset page.
func (r *PostgresRepository) List(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, before *cursor, limit int) ([]Notification, error) {
	var (
		beforeTime any
		beforeID   any
	)
	if before != nil {
		beforeTime = before.CreatedAt
		beforeID = before.ID
	}

	rows, err := tx.Query(ctx, listSQL, tenantID, userID, beforeTime, beforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("notify: list: %w", err)
	}
	defer rows.Close()

	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Notification, error) {
		var n Notification
		err := row.Scan(&n.ID, &n.UserID, &n.Kind, &n.Title, &n.Body, &n.Link, &n.ReadAt, &n.CreatedAt)
		return n, err
	})
	if err != nil {
		return nil, fmt.Errorf("notify: scan: %w", err)
	}
	return out, nil
}

// UnreadCount counts the unread rows through the partial index.
func (r *PostgresRepository) UnreadCount(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) (int, error) {
	var count int
	err := tx.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE tenant_id = $1 AND user_id = $2 AND read_at IS NULL`,
		tenantID, userID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("notify: unread count: %w", err)
	}
	return count, nil
}

const markReadSQL = `
	UPDATE notifications SET read_at = now()
	WHERE tenant_id = $1 AND user_id = $2 AND id = $3 AND read_at IS NULL`

// MarkRead marks one notification read. The user_id in the predicate is what
// makes "not there" and "not yours" one row count: zero. Already-read is also
// zero rows, which is fine — the caller only needs to know the row exists, and a
// re-mark of a read notice is not a failure worth distinguishing.
func (r *PostgresRepository) MarkRead(ctx context.Context, tx pgx.Tx, tenantID, userID, id uuid.UUID) (bool, error) {
	tag, err := tx.Exec(ctx, markReadSQL, tenantID, userID, id)
	if err != nil {
		return false, fmt.Errorf("notify: mark read: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}

	// Zero rows: either it is not the caller's, or it was already read. Distinguish
	// the two so an already-read notice is success, not a 404.
	var exists bool
	err = tx.QueryRow(ctx,
		`SELECT true FROM notifications WHERE tenant_id = $1 AND user_id = $2 AND id = $3`,
		tenantID, userID, id).Scan(&exists)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("notify: confirm read: %w", err)
	}
	return exists, nil
}

// MarkAllRead clears the caller's unread notifications in one statement.
func (r *PostgresRepository) MarkAllRead(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) error {
	_, err := tx.Exec(ctx,
		`UPDATE notifications SET read_at = now() WHERE tenant_id = $1 AND user_id = $2 AND read_at IS NULL`,
		tenantID, userID)
	if err != nil {
		return fmt.Errorf("notify: mark all read: %w", err)
	}
	return nil
}

// fanOutSQL notifies every enrolled learner of a course in one idempotent
// statement. The notification id is derived from the announcement and the
// recipient, so a retried job — River retries on failure — inserts nothing new
// rather than a duplicate bell for everyone it already reached.
//
// It reads the enrolments table directly, the shared-schema read the learn and
// forum packages also use; notify still imports no sibling. A learner who enrols
// after the job has run does not get the old announcement, which is correct: an
// announcement reaches those enrolled when it was posted.
const fanOutSQL = `
	INSERT INTO notifications (id, tenant_id, user_id, kind, title, body, link)
	SELECT md5($3::text || e.user_id::text)::uuid, $1, e.user_id, $4, $5, $6, $7
	FROM enrolments e
	WHERE e.tenant_id = $1 AND e.course_id = $2 AND e.status IN ('active', 'completed')
	ON CONFLICT (id) DO NOTHING`

// FanOutAnnouncement writes one notification per enrolled learner, returning how
// many were newly created.
func (r *PostgresRepository) FanOutAnnouncement(ctx context.Context, tx pgx.Tx, tenantID, courseID, announcementID uuid.UUID, title, body, link string) (int, error) {
	tag, err := tx.Exec(ctx, fanOutSQL, tenantID, courseID, announcementID, KindAnnouncement, title, body, link)
	if err != nil {
		return 0, fmt.Errorf("notify: fan out announcement: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
