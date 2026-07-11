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

// AllTenantIDs lists every tenant, read unbound. The tenants table is not
// tenant-scoped, so this is legible without binding — which the digest needs,
// since it then binds each tenant in turn to read its notifications.
func (r *PostgresRepository) AllTenantIDs(ctx context.Context, tx pgx.Tx) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM tenants ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("notify: list tenants: %w", err)
	}
	defer rows.Close()

	ids, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (uuid.UUID, error) {
		var id uuid.UUID
		err := row.Scan(&id)
		return id, err
	})
	if err != nil {
		return nil, fmt.Errorf("notify: scan tenants: %w", err)
	}
	return ids, nil
}

const pendingDigestSQL = `
	SELECT n.user_id, COALESCE(u.email, ''), COALESCE(u.name, ''),
	       n.id, n.kind, n.title, n.body, n.link, n.read_at, n.created_at
	FROM notifications n
	JOIN users u ON u.id = n.user_id
	LEFT JOIN notification_preferences p ON p.tenant_id = n.tenant_id AND p.user_id = n.user_id
	WHERE n.tenant_id = $1 AND n.read_at IS NULL AND n.digested_at IS NULL
	  AND COALESCE(p.email_digest, true)
	ORDER BY n.user_id, n.created_at`

// PendingDigests returns a tenant's unread, not-yet-digested notifications grouped
// by recipient — one query, grouped in Go, so a hundred waiting people is not a
// hundred queries. Anyone who has turned the digest off is left out by the join.
func (r *PostgresRepository) PendingDigests(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]DigestGroup, error) {
	rows, err := tx.Query(ctx, pendingDigestSQL, tenantID)
	if err != nil {
		return nil, fmt.Errorf("notify: pending digests: %w", err)
	}
	defer rows.Close()

	var (
		groups []DigestGroup
		byUser = map[uuid.UUID]int{} // user id -> index into groups
	)
	for rows.Next() {
		var (
			userID      uuid.UUID
			email, name string
			n           Notification
		)
		if err := rows.Scan(&userID, &email, &name, &n.ID, &n.Kind, &n.Title, &n.Body, &n.Link, &n.ReadAt, &n.CreatedAt); err != nil {
			return nil, fmt.Errorf("notify: scan pending digest: %w", err)
		}
		n.UserID = userID
		idx, ok := byUser[userID]
		if !ok {
			byUser[userID] = len(groups)
			groups = append(groups, DigestGroup{UserID: userID, Email: email, Name: name})
			idx = len(groups) - 1
		}
		groups[idx].Notifications = append(groups[idx].Notifications, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notify: iterate pending digests: %w", err)
	}
	return groups, nil
}

// MarkDigested stamps the given notifications, so a retried sweep re-mails no one.
func (r *PostgresRepository) MarkDigested(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx,
		`UPDATE notifications SET digested_at = now()
		 WHERE tenant_id = $1 AND id = ANY($2) AND digested_at IS NULL`,
		tenantID, ids)
	if err != nil {
		return fmt.Errorf("notify: mark digested: %w", err)
	}
	return nil
}

// Preferences reads a person's settings, or the defaults when they have no row.
func (r *PostgresRepository) Preferences(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) (Preferences, error) {
	prefs := DefaultPreferences()
	err := tx.QueryRow(ctx,
		`SELECT email_digest FROM notification_preferences WHERE tenant_id = $1 AND user_id = $2`,
		tenantID, userID).Scan(&prefs.EmailDigest)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DefaultPreferences(), nil
		}
		return Preferences{}, fmt.Errorf("notify: read preferences: %w", err)
	}
	return prefs, nil
}

// SetPreferences upserts a person's settings.
func (r *PostgresRepository) SetPreferences(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, p Preferences) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO notification_preferences (tenant_id, user_id, email_digest)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (tenant_id, user_id) DO UPDATE SET email_digest = EXCLUDED.email_digest, updated_at = now()`,
		tenantID, userID, p.EmailDigest)
	if err != nil {
		return fmt.Errorf("notify: write preferences: %w", err)
	}
	return nil
}
