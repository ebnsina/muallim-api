package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/platform/database"
)

// Repository is where notifications live. Every method takes a transaction,
// never the pool: app.tenant_id is bound transaction-locally.
type Repository interface {
	// Insert writes one notification. Called by a producer inside its own
	// transaction, so the notice and the event it describes commit together.
	Insert(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n Notification) error

	// List returns one keyset page of a person's notifications, newest first,
	// asking for limit+1 so the caller can tell whether a next page exists.
	List(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, before *cursor, limit int) ([]Notification, error)

	// UnreadCount is the person's unread total, from the partial index.
	UnreadCount(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) (int, error)

	// MarkRead marks one notification read, scoped to its owner. Returns whether a
	// row was theirs to mark.
	MarkRead(ctx context.Context, tx pgx.Tx, tenantID, userID, id uuid.UUID) (bool, error)

	// MarkAllRead marks every unread notification of a person read.
	MarkAllRead(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) error

	// FanOutAnnouncement writes one notification per enrolled learner of a course,
	// idempotently, returning how many were newly created.
	FanOutAnnouncement(ctx context.Context, tx pgx.Tx, tenantID, courseID, announcementID uuid.UUID, title, body, link string) (int, error)

	// AllTenantIDs lists every tenant, read unbound — the digest sweep visits each,
	// because the notifications it reads are behind each tenant's own RLS.
	AllTenantIDs(ctx context.Context, tx pgx.Tx) ([]uuid.UUID, error)

	// PendingDigests groups a tenant's unread, not-yet-digested notifications by
	// recipient, skipping anyone who has turned the digest off. One query; the
	// caller stitches the groups.
	PendingDigests(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]DigestGroup, error)

	// MarkDigested stamps notifications as rolled into a digest, so a retry mails
	// no one twice.
	MarkDigested(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, ids []uuid.UUID) error

	// Preferences reads a person's notification settings, or the defaults.
	Preferences(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) (Preferences, error)

	// SetPreferences writes a person's settings.
	SetPreferences(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, p Preferences) error
}

// DigestMailer enqueues one rendered digest email on the caller's transaction, so
// the email and the "these are digested" stamp commit together. Declared here, by
// the consumer; cmd satisfies it over the comms enqueuer.
type DigestMailer interface {
	SendRendered(ctx context.Context, tx pgx.Tx, to, subject, text string) error
}

// Service holds the rules and owns the transaction boundaries.
type Service struct {
	db     *database.DB
	repo   Repository
	mailer DigestMailer
}

// NewService returns a Service. The mailer is only used by the digest sweep, so a
// nil one is fine everywhere except the worker that runs it.
func NewService(db *database.DB, repo Repository, mailer DigestMailer) *Service {
	return &Service{db: db, repo: repo, mailer: mailer}
}

// Record writes a notification inside the caller's transaction. This is the
// producer entry point — a domain calls it (through an interface it declares)
// while committing the event the notice is about, so the two are atomic. An empty
// recipient is a no-op rather than an error: an event with nobody to tell (a
// question whose author has been erased) simply notifies no one.
func (s *Service) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n Notification) error {
	if n.UserID == uuid.Nil {
		return nil
	}
	return s.repo.Insert(ctx, tx, tenantID, n)
}

// List returns one keyset page of a person's notifications, newest first.
func (s *Service) List(ctx context.Context, tenantID, userID uuid.UUID, token string, limit int) (Page, error) {
	if limit <= 0 || limit > MaxPageSize {
		limit = DefaultPageSize
	}

	var before *cursor
	if token != "" {
		c, err := decodeCursor(token)
		if err != nil {
			return Page{}, err
		}
		before = &c
	}

	var page Page
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// limit+1 to detect a next page without a COUNT.
		rows, err := s.repo.List(ctx, tx, tenantID, userID, before, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			last := rows[limit-1]
			page.NextCursor = cursor{CreatedAt: last.CreatedAt, ID: last.ID}.encode()
			rows = rows[:limit]
		}
		page.Notifications = rows
		return nil
	})
	return page, err
}

// UnreadCount is the number of notifications the person has not yet seen.
func (s *Service) UnreadCount(ctx context.Context, tenantID, userID uuid.UUID) (int, error) {
	var count int
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		count, err = s.repo.UnreadCount(ctx, tx, tenantID, userID)
		return err
	})
	return count, err
}

// MarkRead marks one of the caller's notifications read. Not theirs, or not
// there, is ErrNotFound — one answer, so neither reveals the other.
func (s *Service) MarkRead(ctx context.Context, tenantID, userID, id uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		marked, err := s.repo.MarkRead(ctx, tx, tenantID, userID, id)
		if err != nil {
			return err
		}
		if !marked {
			return ErrNotFound
		}
		return nil
	})
}

// MarkAllRead clears the caller's whole unread count in one statement.
func (s *Service) MarkAllRead(ctx context.Context, tenantID, userID uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.MarkAllRead(ctx, tx, tenantID, userID)
	})
}

// FanOutAnnouncement notifies every enrolled learner of a course that an
// announcement was posted. It owns its own transaction: the job worker runs it
// out of band, after the announcement has already committed, so a slow fan-out
// never holds up the instructor's request. Returns how many were newly notified.
func (s *Service) FanOutAnnouncement(ctx context.Context, tenantID, courseID, announcementID uuid.UUID, title, body, link string) (int, error) {
	var created int
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		created, err = s.repo.FanOutAnnouncement(ctx, tx, tenantID, courseID, announcementID, title, body, link)
		return err
	})
	return created, err
}

// Preferences reads a person's notification settings, or the defaults if they
// have never changed anything.
func (s *Service) Preferences(ctx context.Context, tenantID, userID uuid.UUID) (Preferences, error) {
	var prefs Preferences
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		prefs, err = s.repo.Preferences(ctx, tx, tenantID, userID)
		return err
	})
	return prefs, err
}

// SetPreferences writes a person's notification settings.
func (s *Service) SetPreferences(ctx context.Context, tenantID, userID uuid.UUID, p Preferences) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.SetPreferences(ctx, tx, tenantID, userID, p)
	})
}

// SendDigests mails each person a summary of their unread notifications and
// stamps those notifications as digested. It runs once a day, from the worker.
//
// It visits every tenant, binding each in turn: the notifications it reads live
// behind each tenant's own row-level security, so an unbound sweep would see
// nothing. Per tenant it is one transaction — the emails are enqueued and the
// notifications stamped together, so a retry after a crash re-sends to no one who
// was already reached. Returns how many people were mailed.
func (s *Service) SendDigests(ctx context.Context) (int, error) {
	if s.mailer == nil {
		return 0, errors.New("notify: the digest needs a mailer")
	}

	var tenants []uuid.UUID
	if err := s.db.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		tenants, err = s.repo.AllTenantIDs(ctx, tx)
		return err
	}); err != nil {
		return 0, err
	}

	sent := 0
	for _, tenantID := range tenants {
		mailed, err := s.digestTenant(ctx, tenantID)
		if err != nil {
			return sent, err
		}
		sent += mailed
	}
	return sent, nil
}

func (s *Service) digestTenant(ctx context.Context, tenantID uuid.UUID) (int, error) {
	mailed := 0
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		groups, err := s.repo.PendingDigests(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		for _, g := range groups {
			if g.Email == "" || len(g.Notifications) == 0 {
				continue
			}
			subject, text := renderDigest(g)
			if err := s.mailer.SendRendered(ctx, tx, g.Email, subject, text); err != nil {
				return err
			}
			ids := make([]uuid.UUID, len(g.Notifications))
			for i, n := range g.Notifications {
				ids[i] = n.ID
			}
			if err := s.repo.MarkDigested(ctx, tx, tenantID, ids); err != nil {
				return err
			}
			mailed++
		}
		return nil
	})
	return mailed, err
}

// renderDigest turns a person's pending notifications into a plain-text email.
func renderDigest(g DigestGroup) (subject, text string) {
	n := len(g.Notifications)
	if n == 1 {
		subject = "You have a new notification"
	} else {
		subject = fmt.Sprintf("You have %d new notifications", n)
	}

	name := g.Name
	if name == "" {
		name = "there"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Hi %s,\n\nHere is what you missed:\n\n", name)
	for _, item := range g.Notifications {
		fmt.Fprintf(&b, "  • %s", item.Title)
		if item.Body != "" {
			fmt.Fprintf(&b, " — %s", item.Body)
		}
		b.WriteString("\n")
	}
	b.WriteString("\nSign in to read them. You can turn this digest off in your notification settings.\n")
	return subject, b.String()
}
