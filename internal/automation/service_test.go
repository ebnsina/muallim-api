package automation_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/automation"
	"github.com/ebnsina/muallim-api/internal/platform/database"
)

func testDB(t *testing.T) *database.DB {
	t.Helper()
	url := os.Getenv("MUALLIM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MUALLIM_TEST_DATABASE_URL is not set")
	}
	db, err := database.New(t.Context(), database.Options{URL: url, MaxConns: 4},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func seedTenant(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	sub := "a" + id.String()[:8]
	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, subdomain, name) VALUES ($1, $2, $3)`, id, sub, "Test "+sub)
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_ = db.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, id)
			return err
		})
	})
	return id
}

// sentMessage is one email the mailer was asked to send.
type sentMessage struct{ To, Subject, Text string }

// recordingMailer stands in for comms. The queue is somebody else's test; what
// matters here is what was composed and who it was addressed to.
type recordingMailer struct {
	sent []sentMessage
	err  error
}

func (m *recordingMailer) SendRendered(_ context.Context, _ pgx.Tx, to, subject, text string) error {
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, sentMessage{To: to, Subject: subject, Text: text})
	return nil
}

func newService(db *database.DB, mailer automation.Mailer) *automation.Service {
	return automation.NewService(db, automation.NewPostgresRepository(), mailer, nil)
}

// A rule fires only for its own event, only when enabled, and its placeholders
// are filled from what happened.
func TestFireSendsEnabledRulesForTheEvent(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	mailer := &recordingMailer{}
	svc := newService(db, mailer)
	author := automation.Author{UserID: uuid.New()}

	if _, err := svc.Create(t.Context(), tenant, automation.NewRule{
		Event:   automation.EventEnrolled,
		Subject: "Welcome to {{course_title}}",
		Body:    "Hello {{learner_name}}, you are on {{course_title}}.",
		Enabled: true,
	}, author); err != nil {
		t.Fatalf("create welcome: %v", err)
	}

	// Disabled: written but not switched on. It must not fire.
	if _, err := svc.Create(t.Context(), tenant, automation.NewRule{
		Event: automation.EventEnrolled, Subject: "Draft", Body: "Not ready", Enabled: false,
	}, author); err != nil {
		t.Fatalf("create draft: %v", err)
	}

	// A rule for the other event. It must not fire on this one.
	if _, err := svc.Create(t.Context(), tenant, automation.NewRule{
		Event: automation.EventCompleted, Subject: "Well done", Body: "Finished!", Enabled: true,
	}, author); err != nil {
		t.Fatalf("create congratulations: %v", err)
	}

	err := db.WithTenant(t.Context(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return svc.Fire(ctx, tx, tenant, automation.EventEnrolled, automation.Message{
			To:   "learner@example.com",
			Vars: map[string]string{"learner_name": "Ayesha", "course_title": "Tajweed"},
		})
	})
	if err != nil {
		t.Fatalf("fire: %v", err)
	}

	if len(mailer.sent) != 1 {
		t.Fatalf("sent %d messages, want 1 (the enabled rule for this event alone): %+v", len(mailer.sent), mailer.sent)
	}
	got := mailer.sent[0]
	if got.To != "learner@example.com" {
		t.Errorf("addressed to %q", got.To)
	}
	if got.Subject != "Welcome to Tajweed" {
		t.Errorf("subject %q, want the placeholder filled", got.Subject)
	}
	if got.Text != "Hello Ayesha, you are on Tajweed." {
		t.Errorf("body %q, want both placeholders filled", got.Text)
	}
}

// A learner with no address is not an error. It is a workspace that never
// collected one, and nothing is sent.
func TestFireWithNobodyToSendToSendsNothing(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	mailer := &recordingMailer{}
	svc := newService(db, mailer)

	if _, err := svc.Create(t.Context(), tenant, automation.NewRule{
		Event: automation.EventEnrolled, Subject: "Welcome", Body: "Hello", Enabled: true,
	}, automation.Author{UserID: uuid.New()}); err != nil {
		t.Fatalf("create: %v", err)
	}

	err := db.WithTenant(t.Context(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return svc.Fire(ctx, tx, tenant, automation.EventEnrolled, automation.Message{To: ""})
	})
	if err != nil {
		t.Fatalf("fire without an address: %v", err)
	}
	if len(mailer.sent) != 0 {
		t.Fatalf("sent %d messages to nobody", len(mailer.sent))
	}
}

// A template naming something its event cannot fill is refused while its author
// is looking at it, rather than rendering a gap in front of a learner.
func TestARuleCannotNameAPlaceholderNothingFills(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db, &recordingMailer{})
	author := automation.Author{UserID: uuid.New()}

	_, err := svc.Create(t.Context(), tenant, automation.NewRule{
		Event: automation.EventEnrolled, Subject: "Welcome",
		Body: "Hello {{learner_name}}, your teacher is {{instructor_phone_number}}.", Enabled: true,
	}, author)
	if !errors.Is(err, automation.ErrInvalid) {
		t.Fatalf("a template naming an unfillable placeholder was accepted: %v", err)
	}

	// An unknown event has no placeholders at all, so it cannot be written for.
	if _, err := svc.Create(t.Context(), tenant, automation.NewRule{
		Event: "course.abandoned", Subject: "Hi", Body: "Hi", Enabled: true,
	}, author); !errors.Is(err, automation.ErrInvalid) {
		t.Fatalf("a rule for an event nothing fires was accepted: %v", err)
	}
}

// The event is fixed when the rule is written: an edit that repointed a rule an
// admin already trusts is not an edit. Editing checks against the event it has.
func TestUpdateChecksTemplatesAgainstTheRulesOwnEvent(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	mailer := &recordingMailer{}
	svc := newService(db, mailer)
	author := automation.Author{UserID: uuid.New()}

	rule, err := svc.Create(t.Context(), tenant, automation.NewRule{
		Event: automation.EventCompleted, Subject: "Well done", Body: "You finished.", Enabled: false,
	}, author)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	bad := "Congratulations {{nobody_knows_this}}"
	if _, err := svc.Update(t.Context(), tenant, rule.ID, automation.RulePatch{Body: &bad}, author); !errors.Is(err, automation.ErrInvalid) {
		t.Fatalf("an edit naming an unfillable placeholder was accepted: %v", err)
	}

	// Switching it on is an edit like any other, and it then fires.
	on := true
	if _, err := svc.Update(t.Context(), tenant, rule.ID, automation.RulePatch{Enabled: &on}, author); err != nil {
		t.Fatalf("enable: %v", err)
	}
	err = db.WithTenant(t.Context(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return svc.Fire(ctx, tx, tenant, automation.EventCompleted, automation.Message{
			To: "learner@example.com", Vars: map[string]string{"learner_name": "Ayesha"},
		})
	})
	if err != nil {
		t.Fatalf("fire: %v", err)
	}
	if len(mailer.sent) != 1 {
		t.Fatalf("an enabled rule did not fire: %+v", mailer.sent)
	}
}

// One workspace's rules are its own. A rule is never fired for a tenant that did
// not write it — RLS is the net, but this is the thing the net is under.
func TestRulesDoNotFireForAnotherWorkspace(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	mine := seedTenant(t, db)
	theirs := seedTenant(t, db)
	mailer := &recordingMailer{}
	svc := newService(db, mailer)

	if _, err := svc.Create(t.Context(), mine, automation.NewRule{
		Event: automation.EventEnrolled, Subject: "Mine", Body: "Mine", Enabled: true,
	}, automation.Author{UserID: uuid.New()}); err != nil {
		t.Fatalf("create: %v", err)
	}

	err := db.WithTenant(t.Context(), theirs, func(ctx context.Context, tx pgx.Tx) error {
		return svc.Fire(ctx, tx, theirs, automation.EventEnrolled, automation.Message{
			To: "someone@example.com", Vars: map[string]string{},
		})
	})
	if err != nil {
		t.Fatalf("fire: %v", err)
	}
	if len(mailer.sent) != 0 {
		t.Fatalf("another workspace's rule fired: %+v", mailer.sent)
	}

	rules, err := svc.List(t.Context(), theirs)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("another workspace's rules were listed: %+v", rules)
	}
}
