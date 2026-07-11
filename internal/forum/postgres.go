package forum

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgresRepository is the forum tables.
type PostgresRepository struct{}

// NewPostgresRepository returns one.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

// nilUUID is the value COALESCE substitutes for a deleted author, so a scan into
// uuid.UUID never sees NULL. uuid.Nil then reads as "no author".
const nilUUID = `'00000000-0000-0000-0000-000000000000'::uuid`

// spaceVisible is the access clause: a space is seen if it is workspace-wide, or
// the caller may moderate, or they hold a live enrolment in its course. Written
// once and interpolated with the right parameter numbers per query, the way the
// learn package guards a lesson.
//
// %[1]s is the space alias's course_id and tenant_id source, %[2]s the caller's
// user id, %[3]s the moderator flag.
const spaceVisibleFmt = `(%[1]s.course_id IS NULL OR %[3]s OR EXISTS (
		SELECT 1 FROM enrolments e
		WHERE e.tenant_id = %[1]s.tenant_id AND e.course_id = %[1]s.course_id AND e.user_id = %[2]s
		  AND e.status IN ('active', 'completed') AND (e.expires_at IS NULL OR e.expires_at > now())
	))`

var spacesSQL = fmt.Sprintf(`
	SELECT s.id, s.course_id, s.title, s.description, s.position, s.created_at,
	       COALESCE(c.slug, ''), COALESCE(c.title, ''), COALESCE(tc.n, 0)
	FROM forum_spaces s
	LEFT JOIN courses c ON c.id = s.course_id
	LEFT JOIN (
		SELECT space_id, count(*) AS n FROM forum_threads WHERE tenant_id = $1 GROUP BY space_id
	) tc ON tc.space_id = s.id
	WHERE s.tenant_id = $1 AND `+spaceVisibleFmt+`
	ORDER BY s.position, s.id`, "s", "$2", "$3")

// Spaces lists the boards visible to the actor, with thread counts. The counts
// come from one grouped aggregate, not a subquery per space.
func (r *PostgresRepository) Spaces(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, actor Actor) ([]Space, error) {
	rows, err := tx.Query(ctx, spacesSQL, tenantID, actor.UserID, actor.CanModerate)
	if err != nil {
		return nil, fmt.Errorf("forum: list spaces: %w", err)
	}
	defer rows.Close()

	out, err := pgx.CollectRows(rows, scanSpace)
	if err != nil {
		return nil, fmt.Errorf("forum: scan spaces: %w", err)
	}
	return out, nil
}

func scanSpace(row pgx.CollectableRow) (Space, error) {
	var s Space
	err := row.Scan(&s.ID, &s.CourseID, &s.Title, &s.Desc, &s.Position, &s.CreatedAt,
		&s.CourseSlug, &s.CourseTitle, &s.ThreadCount)
	return s, err
}

var spaceSQL = fmt.Sprintf(`
	SELECT s.id, s.course_id, s.title, s.description, s.position, s.created_at,
	       COALESCE(c.slug, ''), COALESCE(c.title, ''),
	       (SELECT count(*) FROM forum_threads t WHERE t.tenant_id = s.tenant_id AND t.space_id = s.id)
	FROM forum_spaces s
	LEFT JOIN courses c ON c.id = s.course_id
	WHERE s.tenant_id = $1 AND s.id = $2 AND `+spaceVisibleFmt, "s", "$3", "$4")

// Space returns one visible space, or ErrSpaceNotFound.
func (r *PostgresRepository) Space(ctx context.Context, tx pgx.Tx, tenantID, spaceID uuid.UUID, actor Actor) (Space, error) {
	rows, err := tx.Query(ctx, spaceSQL, tenantID, spaceID, actor.UserID, actor.CanModerate)
	if err != nil {
		return Space{}, fmt.Errorf("forum: load space: %w", err)
	}
	space, err := pgx.CollectExactlyOneRow(rows, scanSpace)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Space{}, ErrSpaceNotFound
		}
		return Space{}, fmt.Errorf("forum: load space: %w", err)
	}
	return space, nil
}

// CourseIDBySlug resolves a course to bind a space to.
func (r *PostgresRepository) CourseIDBySlug(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT id FROM courses WHERE tenant_id = $1 AND lower(slug) = lower($2)`,
		tenantID, slug).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrSpaceNotFound
		}
		return uuid.Nil, fmt.Errorf("forum: resolve course: %w", err)
	}
	return id, nil
}

const createSpaceSQL = `
	INSERT INTO forum_spaces (tenant_id, course_id, title, description)
	VALUES ($1, $2, $3, $4)
	RETURNING id, course_id, title, description, position, created_at`

// CreateSpace writes a board and returns it. Course slug/title are left blank —
// the caller reloads the list, which fills them.
func (r *PostgresRepository) CreateSpace(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, courseID *uuid.UUID, title, desc string) (Space, error) {
	var s Space
	err := tx.QueryRow(ctx, createSpaceSQL, tenantID, courseID, title, desc).
		Scan(&s.ID, &s.CourseID, &s.Title, &s.Desc, &s.Position, &s.CreatedAt)
	if err != nil {
		return Space{}, fmt.Errorf("forum: create space: %w", err)
	}
	return s, nil
}

const ThreadsSQL = `
	SELECT t.id, t.space_id, COALESCE(t.author_id, ` + nilUUID + `), COALESCE(u.name, ''),
	       t.title, t.body, t.pinned, t.locked, t.reply_count, t.last_activity_at, t.created_at
	FROM forum_threads t
	LEFT JOIN users u ON u.id = t.author_id
	WHERE t.tenant_id = $1 AND t.space_id = $2
	  AND ($3::boolean IS NULL OR (t.pinned, t.last_activity_at, t.id) < ($3, $4, $5))
	ORDER BY t.pinned DESC, t.last_activity_at DESC, t.id DESC
	LIMIT $6`

// Threads returns one keyset page of a space's threads, pinned first.
func (r *PostgresRepository) Threads(ctx context.Context, tx pgx.Tx, tenantID, spaceID uuid.UUID, before *threadCursor, limit int) ([]Thread, error) {
	var (
		pinned any
		act    any
		id     any
	)
	if before != nil {
		pinned = before.Pinned
		act = before.Activity
		id = before.ID
	}

	rows, err := tx.Query(ctx, ThreadsSQL, tenantID, spaceID, pinned, act, id, limit)
	if err != nil {
		return nil, fmt.Errorf("forum: list threads: %w", err)
	}
	defer rows.Close()

	out, err := pgx.CollectRows(rows, scanThread)
	if err != nil {
		return nil, fmt.Errorf("forum: scan threads: %w", err)
	}
	return out, nil
}

func scanThread(row pgx.CollectableRow) (Thread, error) {
	var t Thread
	err := row.Scan(&t.ID, &t.SpaceID, &t.AuthorID, &t.AuthorName, &t.Title, &t.Body,
		&t.Pinned, &t.Locked, &t.ReplyCount, &t.LastActivityAt, &t.CreatedAt)
	return t, err
}

var threadSQL = fmt.Sprintf(`
	SELECT t.id, t.space_id, COALESCE(t.author_id, `+nilUUID+`), COALESCE(u.name, ''),
	       t.title, t.body, t.pinned, t.locked, t.reply_count, t.last_activity_at, t.created_at
	FROM forum_threads t
	JOIN forum_spaces s ON s.id = t.space_id
	LEFT JOIN users u ON u.id = t.author_id
	WHERE t.tenant_id = $1 AND t.id = $2 AND `+spaceVisibleFmt, "s", "$3", "$4")

// Thread returns one thread if its space is visible to the actor.
func (r *PostgresRepository) Thread(ctx context.Context, tx pgx.Tx, tenantID, threadID uuid.UUID, actor Actor) (Thread, error) {
	rows, err := tx.Query(ctx, threadSQL, tenantID, threadID, actor.UserID, actor.CanModerate)
	if err != nil {
		return Thread{}, fmt.Errorf("forum: load thread: %w", err)
	}
	thread, err := pgx.CollectExactlyOneRow(rows, scanThread)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Thread{}, ErrThreadNotFound
		}
		return Thread{}, fmt.Errorf("forum: load thread: %w", err)
	}
	return thread, nil
}

const createThreadSQL = `
	INSERT INTO forum_threads (tenant_id, space_id, author_id, title, body, last_activity_at)
	VALUES ($1, $2, $3, $4, $5, now())
	RETURNING id, space_id, title, body, pinned, locked, reply_count, last_activity_at, created_at`

// CreateThread opens a thread.
func (r *PostgresRepository) CreateThread(ctx context.Context, tx pgx.Tx, tenantID, spaceID, authorID uuid.UUID, title, body string) (Thread, error) {
	t := Thread{AuthorID: authorID}
	err := tx.QueryRow(ctx, createThreadSQL, tenantID, spaceID, authorID, title, body).
		Scan(&t.ID, &t.SpaceID, &t.Title, &t.Body, &t.Pinned, &t.Locked, &t.ReplyCount, &t.LastActivityAt, &t.CreatedAt)
	if err != nil {
		return Thread{}, fmt.Errorf("forum: create thread: %w", err)
	}
	return t, nil
}

const PostsSQL = `
	SELECT p.id, p.thread_id, COALESCE(p.author_id, ` + nilUUID + `), COALESCE(u.name, ''),
	       p.body, p.created_at
	FROM forum_posts p
	LEFT JOIN users u ON u.id = p.author_id
	WHERE p.tenant_id = $1 AND p.thread_id = $2
	  AND ($3::timestamptz IS NULL OR (p.created_at, p.id) > ($3, $4))
	ORDER BY p.created_at, p.id
	LIMIT $5`

// Posts returns one keyset page of a thread's replies, oldest first.
func (r *PostgresRepository) Posts(ctx context.Context, tx pgx.Tx, tenantID, threadID uuid.UUID, before *postCursor, limit int) ([]Post, error) {
	var (
		ts any
		id any
	)
	if before != nil {
		ts = before.CreatedAt
		id = before.ID
	}

	rows, err := tx.Query(ctx, PostsSQL, tenantID, threadID, ts, id, limit)
	if err != nil {
		return nil, fmt.Errorf("forum: list posts: %w", err)
	}
	defer rows.Close()

	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Post, error) {
		var p Post
		err := row.Scan(&p.ID, &p.ThreadID, &p.AuthorID, &p.AuthorName, &p.Body, &p.CreatedAt)
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("forum: scan posts: %w", err)
	}
	return out, nil
}

// Reply writes a post and advances the thread roll-up in the same transaction, so
// the reply count and last-activity never disagree with the posts they summarise.
func (r *PostgresRepository) Reply(ctx context.Context, tx pgx.Tx, tenantID, threadID, authorID uuid.UUID, body string) (Post, error) {
	p := Post{ThreadID: threadID, AuthorID: authorID}
	err := tx.QueryRow(ctx,
		`INSERT INTO forum_posts (tenant_id, thread_id, author_id, body)
		 VALUES ($1, $2, $3, $4) RETURNING id, body, created_at`,
		tenantID, threadID, authorID, body).Scan(&p.ID, &p.Body, &p.CreatedAt)
	if err != nil {
		return Post{}, fmt.Errorf("forum: reply: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE forum_threads SET reply_count = reply_count + 1, last_activity_at = now()
		 WHERE tenant_id = $1 AND id = $2`,
		tenantID, threadID); err != nil {
		return Post{}, fmt.Errorf("forum: bump thread activity: %w", err)
	}
	return p, nil
}

// SetPinned toggles a thread's pin, returning whether it existed.
func (r *PostgresRepository) SetPinned(ctx context.Context, tx pgx.Tx, tenantID, threadID uuid.UUID, pinned bool) (bool, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE forum_threads SET pinned = $3 WHERE tenant_id = $1 AND id = $2`,
		tenantID, threadID, pinned)
	if err != nil {
		return false, fmt.Errorf("forum: set pinned: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// SetLocked toggles a thread's lock.
func (r *PostgresRepository) SetLocked(ctx context.Context, tx pgx.Tx, tenantID, threadID uuid.UUID, locked bool) (bool, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE forum_threads SET locked = $3 WHERE tenant_id = $1 AND id = $2`,
		tenantID, threadID, locked)
	if err != nil {
		return false, fmt.Errorf("forum: set locked: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// DeleteThread removes a thread the actor authored, or any when they moderate.
// The predicate makes "not there", "not yours", and "not a moderator" one row
// count: zero. A moderator's delete ignores authorship.
func (r *PostgresRepository) DeleteThread(ctx context.Context, tx pgx.Tx, tenantID, threadID uuid.UUID, actor Actor) (bool, error) {
	tag, err := tx.Exec(ctx,
		`DELETE FROM forum_threads WHERE tenant_id = $1 AND id = $2 AND ($3 OR author_id = $4)`,
		tenantID, threadID, actor.CanModerate, actor.UserID)
	if err != nil {
		return false, fmt.Errorf("forum: delete thread: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// DeletePost removes a post under the same rule as a thread.
func (r *PostgresRepository) DeletePost(ctx context.Context, tx pgx.Tx, tenantID, postID uuid.UUID, actor Actor) (bool, error) {
	tag, err := tx.Exec(ctx,
		`DELETE FROM forum_posts WHERE tenant_id = $1 AND id = $2 AND ($3 OR author_id = $4)`,
		tenantID, postID, actor.CanModerate, actor.UserID)
	if err != nil {
		return false, fmt.Errorf("forum: delete post: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
