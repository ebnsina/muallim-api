package forum

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is where the forum lives. Every method takes a transaction, never
// the pool: app.tenant_id is bound transaction-locally. Access — which spaces a
// caller may see — is enforced inside the visible-space queries by a join to
// enrolments, so a repository read never returns a board the caller may not see.
type Repository interface {
	// Spaces lists the boards visible to the actor: every workspace-wide space, and
	// every course space they are enrolled in or may moderate, with thread counts.
	Spaces(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, actor Actor) ([]Space, error)

	// Space returns one visible space, or ErrSpaceNotFound.
	Space(ctx context.Context, tx pgx.Tx, tenantID, spaceID uuid.UUID, actor Actor) (Space, error)

	// CourseIDBySlug resolves a course a space may bind to; ErrSpaceNotFound stands
	// in for "no such course", so creating a space against a mistyped slug reads the
	// same as any other missing target.
	CourseIDBySlug(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string) (uuid.UUID, error)

	// CreateSpace writes a board. Caller has already been checked for moderation.
	CreateSpace(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, courseID *uuid.UUID, title, desc string) (Space, error)

	// Threads returns one keyset page of a space's threads, pinned first then most
	// recently active. The space's visibility is checked by the caller first.
	Threads(ctx context.Context, tx pgx.Tx, tenantID, spaceID uuid.UUID, before *threadCursor, limit int) ([]Thread, error)

	// Thread returns one thread if its space is visible to the actor, else
	// ErrThreadNotFound.
	Thread(ctx context.Context, tx pgx.Tx, tenantID, threadID uuid.UUID, actor Actor) (Thread, error)

	// CreateThread opens a thread. The space's visibility is checked by the caller.
	CreateThread(ctx context.Context, tx pgx.Tx, tenantID, spaceID, authorID uuid.UUID, title, body string) (Thread, error)

	// Posts returns one keyset page of a thread's replies, oldest first.
	Posts(ctx context.Context, tx pgx.Tx, tenantID, threadID uuid.UUID, before *postCursor, limit int) ([]Post, error)

	// Reply writes a post and advances the thread's roll-up — reply_count and
	// last_activity_at — in the same call, so the tally never disagrees with the
	// rows it counts.
	Reply(ctx context.Context, tx pgx.Tx, tenantID, threadID, authorID uuid.UUID, body string) (Post, error)

	// SetPinned and SetLocked toggle a thread's flags, returning whether it existed.
	SetPinned(ctx context.Context, tx pgx.Tx, tenantID, threadID uuid.UUID, pinned bool) (bool, error)
	SetLocked(ctx context.Context, tx pgx.Tx, tenantID, threadID uuid.UUID, locked bool) (bool, error)

	// DeleteThread removes a thread the actor authored, or any when they moderate.
	DeleteThread(ctx context.Context, tx pgx.Tx, tenantID, threadID uuid.UUID, actor Actor) (bool, error)

	// DeletePost removes a post the actor authored, or any when they moderate.
	DeletePost(ctx context.Context, tx pgx.Tx, tenantID, postID uuid.UUID, actor Actor) (bool, error)
}

// Notifier delivers a Notification inside the caller's transaction, so a reply
// and its notice commit together. Declared here, by the consumer; wired in cmd.
type Notifier interface {
	Notify(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n Notification) error
}

// Service holds the rules and owns the transaction boundaries.
type Service struct {
	db       *database.DB
	repo     Repository
	notifier Notifier
}

// NewService returns a Service. A nil notifier sends nothing.
func NewService(db *database.DB, repo Repository, notifier Notifier) *Service {
	return &Service{db: db, repo: repo, notifier: notifier}
}

// Spaces lists the boards a caller may see.
func (s *Service) Spaces(ctx context.Context, tenantID uuid.UUID, actor Actor) ([]Space, error) {
	var spaces []Space
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		spaces, err = s.repo.Spaces(ctx, tx, tenantID, actor)
		return err
	})
	return spaces, err
}

// CreateSpace opens a board. A course slug binds it to that course; empty makes
// it workspace-wide. The caller must moderate — checked by the transport layer.
func (s *Service) CreateSpace(ctx context.Context, tenantID uuid.UUID, courseSlug, title, desc string) (Space, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Space{}, ErrEmpty
	}
	if len(title) > MaxTitleLength || len(desc) > MaxBodyLength {
		return Space{}, ErrTooLong
	}

	var space Space
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var courseID *uuid.UUID
		if slug := strings.TrimSpace(courseSlug); slug != "" {
			id, err := s.repo.CourseIDBySlug(ctx, tx, tenantID, slug)
			if err != nil {
				return err
			}
			courseID = &id
		}
		var err error
		space, err = s.repo.CreateSpace(ctx, tx, tenantID, courseID, title, strings.TrimSpace(desc))
		return err
	})
	return space, err
}

// Threads returns one keyset page of a visible space's threads.
func (s *Service) Threads(ctx context.Context, tenantID, spaceID uuid.UUID, actor Actor, token string, limit int) (Space, ThreadPage, error) {
	limit = clampLimit(limit)

	var before *threadCursor
	if token != "" {
		c, err := decodeThreadCursor(token)
		if err != nil {
			return Space{}, ThreadPage{}, err
		}
		before = &c
	}

	var (
		space Space
		page  ThreadPage
	)
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		// Loading the space enforces access: an invisible one is ErrSpaceNotFound.
		if space, err = s.repo.Space(ctx, tx, tenantID, spaceID, actor); err != nil {
			return err
		}
		rows, err := s.repo.Threads(ctx, tx, tenantID, spaceID, before, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			last := rows[limit-1]
			page.NextCursor = threadCursor{Pinned: last.Pinned, Activity: last.LastActivityAt, ID: last.ID}.encode()
			rows = rows[:limit]
		}
		page.Threads = rows
		return nil
	})
	return space, page, err
}

// StartThread opens a thread in a visible space.
func (s *Service) StartThread(ctx context.Context, tenantID, spaceID uuid.UUID, actor Actor, title, body string) (Thread, error) {
	title = strings.TrimSpace(title)
	body = strings.TrimSpace(body)
	if title == "" || body == "" {
		return Thread{}, ErrEmpty
	}
	if len(title) > MaxTitleLength || len(body) > MaxBodyLength {
		return Thread{}, ErrTooLong
	}

	var thread Thread
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := s.repo.Space(ctx, tx, tenantID, spaceID, actor); err != nil {
			return err
		}
		var err error
		thread, err = s.repo.CreateThread(ctx, tx, tenantID, spaceID, actor.UserID, title, body)
		return err
	})
	return thread, err
}

// Thread returns a thread and one keyset page of its replies, if its space is
// visible to the caller.
func (s *Service) Thread(ctx context.Context, tenantID, threadID uuid.UUID, actor Actor, token string, limit int) (Thread, PostPage, error) {
	limit = clampLimit(limit)

	var before *postCursor
	if token != "" {
		c, err := decodePostCursor(token)
		if err != nil {
			return Thread{}, PostPage{}, err
		}
		before = &c
	}

	var (
		thread Thread
		page   PostPage
	)
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if thread, err = s.repo.Thread(ctx, tx, tenantID, threadID, actor); err != nil {
			return err
		}
		rows, err := s.repo.Posts(ctx, tx, tenantID, threadID, before, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			last := rows[limit-1]
			page.NextCursor = postCursor{CreatedAt: last.CreatedAt, ID: last.ID}.encode()
			rows = rows[:limit]
		}
		page.Posts = rows
		return nil
	})
	return thread, page, err
}

// Reply posts to a thread. A locked thread takes no replies except a moderator's.
// The thread's author is notified, unless they are the one replying.
func (s *Service) Reply(ctx context.Context, tenantID, threadID uuid.UUID, actor Actor, body string) (Post, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return Post{}, ErrEmpty
	}
	if len(body) > MaxBodyLength {
		return Post{}, ErrTooLong
	}

	var post Post
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		thread, err := s.repo.Thread(ctx, tx, tenantID, threadID, actor)
		if err != nil {
			return err
		}
		if thread.Locked && !actor.CanModerate {
			return ErrLocked
		}

		post, err = s.repo.Reply(ctx, tx, tenantID, threadID, actor.UserID, body)
		if err != nil {
			return err
		}

		if s.notifier == nil || thread.AuthorID == uuid.Nil || thread.AuthorID == actor.UserID {
			return nil
		}
		return s.notifier.Notify(ctx, tx, tenantID, Notification{
			UserID: thread.AuthorID,
			Kind:   KindReply,
			Title:  "New reply in your thread",
			Body:   body,
			Link:   "/forum/threads/" + threadID.String(),
		})
	})
	return post, err
}

// Pin and Lock are moderator actions on a thread; the transport layer checks the
// permission, so here they only need the thread to exist.
func (s *Service) Pin(ctx context.Context, tenantID, threadID uuid.UUID, pinned bool) error {
	return s.toggle(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return s.repo.SetPinned(ctx, tx, tenantID, threadID, pinned)
	})
}

func (s *Service) Lock(ctx context.Context, tenantID, threadID uuid.UUID, locked bool) error {
	return s.toggle(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return s.repo.SetLocked(ctx, tx, tenantID, threadID, locked)
	})
}

func (s *Service) toggle(ctx context.Context, tenantID uuid.UUID, fn func(context.Context, pgx.Tx) (bool, error)) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ok, err := fn(ctx, tx)
		if err != nil {
			return err
		}
		if !ok {
			return ErrThreadNotFound
		}
		return nil
	})
}

// DeleteThread removes a thread the caller authored, or any when they moderate.
func (s *Service) DeleteThread(ctx context.Context, tenantID, threadID uuid.UUID, actor Actor) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		removed, err := s.repo.DeleteThread(ctx, tx, tenantID, threadID, actor)
		if err != nil {
			return err
		}
		if !removed {
			return ErrThreadNotFound
		}
		return nil
	})
}

// DeletePost removes a post the caller authored, or any when they moderate.
func (s *Service) DeletePost(ctx context.Context, tenantID, postID uuid.UUID, actor Actor) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		removed, err := s.repo.DeletePost(ctx, tx, tenantID, postID, actor)
		if err != nil {
			return err
		}
		if !removed {
			return ErrPostNotFound
		}
		return nil
	})
}

func clampLimit(limit int) int {
	if limit <= 0 || limit > MaxPageSize {
		return DefaultPageSize
	}
	return limit
}
