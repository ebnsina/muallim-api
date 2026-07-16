package chat

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgresRepository is the chat tables.
type PostgresRepository struct{}

// NewPostgresRepository returns one.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

// nilUUID is what COALESCE substitutes for a null id, so a scan into uuid.UUID
// never sees NULL. uuid.Nil then reads as "nobody".
const nilUUID = `'00000000-0000-0000-0000-000000000000'::uuid`

// convCols is the conversation projection, shared by the read paths.
const convCols = `c.id, c.kind, c.course_id, c.title, COALESCE(c.created_by, ` + nilUUID + `), c.last_message_at, c.created_at`

const insertDirectSQL = `
	INSERT INTO chat_conversations (tenant_id, kind, dm_low, dm_high, created_by)
	VALUES ($1, 'direct', $2, $3, $4)
	ON CONFLICT (tenant_id, dm_low, dm_high) WHERE kind = 'direct' DO NOTHING
	RETURNING ` + convColsBare

// convColsBare is convCols without the "c." alias, for RETURNING off the table.
const convColsBare = `id, kind, course_id, title, COALESCE(created_by, ` + nilUUID + `), last_message_at, created_at`

func scanConversationRow(row pgx.Row) (Conversation, error) {
	var c Conversation
	err := row.Scan(&c.ID, &c.Kind, &c.CourseID, &c.Title, &c.CreatedBy, &c.LastMessageAt, &c.CreatedAt)
	return c, err
}

// EnsureDirect gets or creates the one direct conversation for a canonical pair.
func (r *PostgresRepository) EnsureDirect(ctx context.Context, tx pgx.Tx, tenantID, low, high, creator uuid.UUID) (Conversation, bool, error) {
	c, err := scanConversationRow(tx.QueryRow(ctx, insertDirectSQL, tenantID, low, high, creator))
	if err == nil {
		return c, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Conversation{}, false, fmt.Errorf("chat: ensure direct: %w", err)
	}
	c, err = scanConversationRow(tx.QueryRow(ctx,
		`SELECT `+convColsBare+` FROM chat_conversations
		 WHERE tenant_id = $1 AND kind = 'direct' AND dm_low = $2 AND dm_high = $3`,
		tenantID, low, high))
	if err != nil {
		return Conversation{}, false, fmt.Errorf("chat: load direct: %w", err)
	}
	return c, false, nil
}

const insertCourseChannelSQL = `
	INSERT INTO chat_conversations (tenant_id, kind, course_id, title)
	VALUES ($1, 'course', $2, $3)
	ON CONFLICT (tenant_id, course_id) WHERE kind = 'course' DO NOTHING
	RETURNING ` + convColsBare

// EnsureCourseChannel gets or creates a course's single channel.
func (r *PostgresRepository) EnsureCourseChannel(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, title string) (Conversation, bool, error) {
	c, err := scanConversationRow(tx.QueryRow(ctx, insertCourseChannelSQL, tenantID, courseID, title))
	if err == nil {
		return c, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Conversation{}, false, fmt.Errorf("chat: ensure course channel: %w", err)
	}
	c, err = scanConversationRow(tx.QueryRow(ctx,
		`SELECT `+convColsBare+` FROM chat_conversations
		 WHERE tenant_id = $1 AND kind = 'course' AND course_id = $2`,
		tenantID, courseID))
	if err != nil {
		return Conversation{}, false, fmt.Errorf("chat: load course channel: %w", err)
	}
	return c, false, nil
}

// CreateGroup writes an ad-hoc group conversation.
func (r *PostgresRepository) CreateGroup(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, title string, creator uuid.UUID) (Conversation, error) {
	c, err := scanConversationRow(tx.QueryRow(ctx,
		`INSERT INTO chat_conversations (tenant_id, kind, title, created_by)
		 VALUES ($1, 'group', $2, $3) RETURNING `+convColsBare,
		tenantID, title, creator))
	if err != nil {
		return Conversation{}, fmt.Errorf("chat: create group: %w", err)
	}
	return c, nil
}

// ConversationByID returns one conversation, or ErrNotFound.
func (r *PostgresRepository) ConversationByID(ctx context.Context, tx pgx.Tx, tenantID, convID uuid.UUID) (Conversation, error) {
	c, err := scanConversationRow(tx.QueryRow(ctx,
		`SELECT `+convColsBare+` FROM chat_conversations c WHERE tenant_id = $1 AND id = $2`,
		tenantID, convID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Conversation{}, ErrNotFound
		}
		return Conversation{}, fmt.Errorf("chat: load conversation: %w", err)
	}
	return c, nil
}

// Members lists a conversation's participants with display names, admins first.
func (r *PostgresRepository) Members(ctx context.Context, tx pgx.Tx, tenantID, convID uuid.UUID) ([]Member, error) {
	rows, err := tx.Query(ctx,
		`SELECT m.user_id, COALESCE(u.name, ''), m.role, m.last_read_at
		 FROM chat_members m
		 LEFT JOIN users u ON u.id = m.user_id
		 WHERE m.tenant_id = $1 AND m.conversation_id = $2
		 ORDER BY (m.role = 'admin') DESC, m.created_at, m.user_id`,
		tenantID, convID)
	if err != nil {
		return nil, fmt.Errorf("chat: list members: %w", err)
	}
	defer rows.Close()
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Member, error) {
		var m Member
		err := row.Scan(&m.UserID, &m.Name, &m.Role, &m.LastReadAt)
		return m, err
	})
	if err != nil {
		return nil, fmt.Errorf("chat: scan members: %w", err)
	}
	return out, nil
}

// IsMember reports whether a user belongs to a conversation.
func (r *PostgresRepository) IsMember(ctx context.Context, tx pgx.Tx, tenantID, convID, userID uuid.UUID) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM chat_members
			WHERE tenant_id = $1 AND conversation_id = $2 AND user_id = $3
		)`, tenantID, convID, userID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("chat: is member: %w", err)
	}
	return exists, nil
}

// AddMember adds a participant if absent, reporting whether it was added.
func (r *PostgresRepository) AddMember(ctx context.Context, tx pgx.Tx, tenantID, convID, userID uuid.UUID, role string) (bool, error) {
	tag, err := tx.Exec(ctx,
		`INSERT INTO chat_members (tenant_id, conversation_id, user_id, role)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (tenant_id, conversation_id, user_id) DO NOTHING`,
		tenantID, convID, userID, role)
	if err != nil {
		return false, fmt.Errorf("chat: add member: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RemoveMember drops a participant, reporting whether one was there.
func (r *PostgresRepository) RemoveMember(ctx context.Context, tx pgx.Tx, tenantID, convID, userID uuid.UUID) (bool, error) {
	tag, err := tx.Exec(ctx,
		`DELETE FROM chat_members WHERE tenant_id = $1 AND conversation_id = $2 AND user_id = $3`,
		tenantID, convID, userID)
	if err != nil {
		return false, fmt.Errorf("chat: remove member: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// listForUserSQL loads a member's conversations with the last message and unread
// count in one round trip: two lateral joins, no query per conversation.
const listForUserSQL = `
	SELECT ` + convCols + `,
	       lm.id, COALESCE(lm.sender_id, ` + nilUUID + `), COALESCE(lm.sender_name, ''), lm.body, lm.created_at,
	       COALESCE(unread.n, 0)
	FROM chat_conversations c
	JOIN chat_members me
	  ON me.tenant_id = c.tenant_id AND me.conversation_id = c.id AND me.user_id = $2
	LEFT JOIN LATERAL (
		SELECT m.id, COALESCE(m.sender_id, ` + nilUUID + `) AS sender_id,
		       COALESCE(u.name, '') AS sender_name, m.body, m.created_at
		FROM chat_messages m
		LEFT JOIN users u ON u.id = m.sender_id
		WHERE m.tenant_id = c.tenant_id AND m.conversation_id = c.id
		ORDER BY m.created_at DESC, m.id DESC
		LIMIT 1
	) lm ON true
	LEFT JOIN LATERAL (
		SELECT count(*) AS n
		FROM chat_messages m2
		WHERE m2.tenant_id = c.tenant_id AND m2.conversation_id = c.id
		  AND m2.created_at > COALESCE(me.last_read_at, '-infinity'::timestamptz)
		  AND (m2.sender_id IS NULL OR m2.sender_id <> $2)
	) unread ON true
	WHERE c.tenant_id = $1
	  AND ($3::timestamptz IS NULL OR (c.last_message_at, c.id) < ($3, $4))
	ORDER BY c.last_message_at DESC, c.id DESC
	LIMIT $5`

// ListForUser returns one keyset page of the user's conversations.
func (r *PostgresRepository) ListForUser(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, after *cursor, limit int) ([]Conversation, error) {
	var at, id any
	if after != nil {
		at, id = after.At, after.ID
	}
	rows, err := tx.Query(ctx, listForUserSQL, tenantID, userID, at, id, limit)
	if err != nil {
		return nil, fmt.Errorf("chat: list conversations: %w", err)
	}
	defer rows.Close()

	var out []Conversation
	for rows.Next() {
		var (
			c        Conversation
			lmID     *uuid.UUID
			lmSender uuid.UUID
			lmName   string
			lmBody   *string
			lmAt     *time.Time
			unread   int
		)
		if err := rows.Scan(&c.ID, &c.Kind, &c.CourseID, &c.Title, &c.CreatedBy, &c.LastMessageAt, &c.CreatedAt,
			&lmID, &lmSender, &lmName, &lmBody, &lmAt, &unread); err != nil {
			return nil, fmt.Errorf("chat: scan conversation: %w", err)
		}
		c.Unread = unread
		if lmID != nil && lmBody != nil && lmAt != nil {
			c.LastMessage = &Message{
				ID: *lmID, ConversationID: c.ID, SenderID: lmSender,
				SenderName: lmName, Body: *lmBody, CreatedAt: *lmAt,
			}
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chat: scan conversations: %w", err)
	}
	return out, nil
}

// Messages returns one keyset page of a conversation's messages, newest first.
func (r *PostgresRepository) Messages(ctx context.Context, tx pgx.Tx, tenantID, convID uuid.UUID, after *cursor, limit int) ([]Message, error) {
	var at, id any
	if after != nil {
		at, id = after.At, after.ID
	}
	rows, err := tx.Query(ctx,
		`SELECT m.id, m.conversation_id, COALESCE(m.sender_id, `+nilUUID+`), COALESCE(u.name, ''), m.body, m.created_at
		 FROM chat_messages m
		 LEFT JOIN users u ON u.id = m.sender_id
		 WHERE m.tenant_id = $1 AND m.conversation_id = $2
		   AND ($3::timestamptz IS NULL OR (m.created_at, m.id) < ($3, $4))
		 ORDER BY m.created_at DESC, m.id DESC
		 LIMIT $5`,
		tenantID, convID, at, id, limit)
	if err != nil {
		return nil, fmt.Errorf("chat: list messages: %w", err)
	}
	defer rows.Close()
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Message, error) {
		var m Message
		err := row.Scan(&m.ID, &m.ConversationID, &m.SenderID, &m.SenderName, &m.Body, &m.CreatedAt)
		return m, err
	})
	if err != nil {
		return nil, fmt.Errorf("chat: scan messages: %w", err)
	}
	return out, nil
}

// InsertMessage writes a message and bumps the conversation's activity clock so the
// conversation list ordering never disagrees with the messages it sorts.
func (r *PostgresRepository) InsertMessage(ctx context.Context, tx pgx.Tx, tenantID, convID, senderID uuid.UUID, body string) (Message, error) {
	m := Message{ConversationID: convID, SenderID: senderID, Body: body}
	err := tx.QueryRow(ctx,
		`INSERT INTO chat_messages (tenant_id, conversation_id, sender_id, body)
		 VALUES ($1, $2, $3, $4) RETURNING id, created_at`,
		tenantID, convID, senderID, body).Scan(&m.ID, &m.CreatedAt)
	if err != nil {
		return Message{}, fmt.Errorf("chat: insert message: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE chat_conversations SET last_message_at = $3, updated_at = now()
		 WHERE tenant_id = $1 AND id = $2`,
		tenantID, convID, m.CreatedAt); err != nil {
		return Message{}, fmt.Errorf("chat: bump conversation: %w", err)
	}
	return m, nil
}

// MarkRead stamps the member's last_read_at, reporting whether they were a member.
func (r *PostgresRepository) MarkRead(ctx context.Context, tx pgx.Tx, tenantID, convID, userID uuid.UUID) (bool, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE chat_members SET last_read_at = now()
		 WHERE tenant_id = $1 AND conversation_id = $2 AND user_id = $3`,
		tenantID, convID, userID)
	if err != nil {
		return false, fmt.Errorf("chat: mark read: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// CourseIDBySlug resolves a course so its channel can be opened.
func (r *PostgresRepository) CourseIDBySlug(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT id FROM courses WHERE tenant_id = $1 AND lower(slug) = lower($2)`,
		tenantID, slug).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrNotFound
		}
		return uuid.Nil, fmt.Errorf("chat: resolve course: %w", err)
	}
	return id, nil
}
