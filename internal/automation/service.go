package automation

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the storage this package needs. Every method takes the caller's
// transaction: a rule is read in the transaction of the event it answers.
type Repository interface {
	Create(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, r Rule) (Rule, error)
	ByID(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Rule, error)
	List(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit int) ([]Rule, error)
	Update(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, p RulePatch) (Rule, error)
	Delete(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error

	// Firing lists the enabled rules for one event. The hot path: every enrolment
	// and every completion asks it.
	Firing(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, event string) ([]Rule, error)
}

/*
Mailer enqueues a composed message.

Declared here and satisfied in cmd/ over comms, because a domain may not import a
sibling. It takes the caller's transaction, so the email and the thing it is about
commit together — River is Postgres-backed for exactly this.
*/
type Mailer interface {
	SendRendered(ctx context.Context, tx pgx.Tx, to, subject, text string) error
}

// AuditEntry is one line in the workspace's audit log.
type AuditEntry struct {
	Action   string
	ActorID  uuid.UUID
	RuleID   uuid.UUID
	Event    string
	Metadata map[string]any
}

// AuditRecorder writes the entry in the transaction of the change it describes.
type AuditRecorder interface {
	Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e AuditEntry) error
}

// Actions, as they appear in the audit log.
const (
	ActionCreated = "automation.created"
	ActionUpdated = "automation.updated"
	ActionDeleted = "automation.deleted"
)

// MaxRules bounds a workspace's list. A cap rather than a cursor: these are
// written by hand, a few per event, and a workspace at this limit has a problem
// no page of results would solve.
const MaxRules = 100

// Author is who is making a change.
type Author struct{ UserID uuid.UUID }

// Service is the package's entry point.
type Service struct {
	db     *database.DB
	repo   Repository
	mailer Mailer
	audit  AuditRecorder
}

// NewService builds the service. A nil mailer fires nothing, which is what the
// spec-only build passes.
func NewService(db *database.DB, repo Repository, mailer Mailer, audit AuditRecorder) *Service {
	return &Service{db: db, repo: repo, mailer: mailer, audit: audit}
}

/*
Fire sends every enabled rule for an event, in the caller's transaction.

It never fails the thing it is about. A rule that cannot be rendered or a queue
that will not take the message is a problem with the email, and refusing the
enrolment over it would be a learner turned away from a course because a welcome
note misfired. The error is returned for the caller to log; the caller commits
regardless.

Nobody to send to means nothing to send: a learner with no address on file is not
an error, it is a workspace that never collected one.
*/
func (s *Service) Fire(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, event string, m Message) error {
	if s.mailer == nil || m.To == "" {
		return nil
	}

	rules, err := s.repo.Firing(ctx, tx, tenantID, event)
	if err != nil {
		return fmt.Errorf("automation: rules for %q: %w", event, err)
	}

	for _, rule := range rules {
		subject := render(rule.Subject, m.Vars)
		body := render(rule.Body, m.Vars)
		if err := s.mailer.SendRendered(ctx, tx, m.To, subject, body); err != nil {
			return fmt.Errorf("automation: send %q: %w", rule.Event, err)
		}
	}
	return nil
}

// Create writes a rule.
func (s *Service) Create(ctx context.Context, tenantID uuid.UUID, n NewRule, author Author) (Rule, error) {
	if err := n.validate(); err != nil {
		return Rule{}, err
	}

	var created Rule
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		created, err = s.repo.Create(ctx, tx, tenantID, Rule{
			Event: n.Event, Subject: n.Subject, Body: n.Body, Enabled: n.Enabled,
		})
		if err != nil {
			return err
		}
		return s.record(ctx, tx, tenantID, AuditEntry{
			Action: ActionCreated, ActorID: author.UserID,
			RuleID: created.ID, Event: created.Event,
			Metadata: map[string]any{"enabled": created.Enabled},
		})
	})
	return created, err
}

// List returns a workspace's rules, newest first.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID) ([]Rule, error) {
	var rules []Rule
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rules, err = s.repo.List(ctx, tx, tenantID, MaxRules)
		return err
	})
	return rules, err
}

// Get returns one rule.
func (s *Service) Get(ctx context.Context, tenantID, id uuid.UUID) (Rule, error) {
	var rule Rule
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rule, err = s.repo.ByID(ctx, tx, tenantID, id)
		return err
	})
	return rule, err
}

// Update applies a patch. The event is fixed at creation, so the templates are
// checked against the event the rule already has.
func (s *Service) Update(ctx context.Context, tenantID, id uuid.UUID, p RulePatch, author Author) (Rule, error) {
	var updated Rule
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		existing, err := s.repo.ByID(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if err := p.validate(existing.Event); err != nil {
			return err
		}

		updated, err = s.repo.Update(ctx, tx, tenantID, id, p)
		if err != nil {
			return err
		}
		return s.record(ctx, tx, tenantID, AuditEntry{
			Action: ActionUpdated, ActorID: author.UserID,
			RuleID: updated.ID, Event: updated.Event,
			Metadata: map[string]any{"enabled": updated.Enabled},
		})
	})
	return updated, err
}

// Delete removes a rule.
func (s *Service) Delete(ctx context.Context, tenantID, id uuid.UUID, author Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		existing, err := s.repo.ByID(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if err := s.repo.Delete(ctx, tx, tenantID, id); err != nil {
			return err
		}
		return s.record(ctx, tx, tenantID, AuditEntry{
			Action: ActionDeleted, ActorID: author.UserID,
			RuleID: id, Event: existing.Event,
		})
	})
}

func (s *Service) record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e AuditEntry) error {
	if s.audit == nil {
		return nil
	}
	return s.audit.Record(ctx, tx, tenantID, e)
}
