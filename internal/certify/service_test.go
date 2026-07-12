package certify_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/audit"
	"github.com/ebnsina/muallim-api/internal/certify"
	"github.com/ebnsina/muallim-api/internal/platform/database"
)

func testDB(t *testing.T) *database.DB {
	t.Helper()

	url := os.Getenv("LMS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LMS_TEST_DATABASE_URL is not set; skipping database tests")
	}

	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	db, err := database.New(t.Context(), database.Options{URL: url, MaxConns: 4}, log)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

type auditor struct{ recorder *audit.Recorder }

func (a auditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e certify.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID: e.ActorID, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID, Metadata: e.Metadata,
	})
}

type fixture struct {
	db     *database.DB
	svc    *certify.Service
	tenant uuid.UUID
	course uuid.UUID
	slug   string
}

func newFixture(t *testing.T) fixture {
	t.Helper()

	db := testDB(t)

	tenantID := uuid.New()
	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, subdomain, name) VALUES ($1, $2, 'Test')`,
			tenantID, "t"+tenantID.String()[:8])
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_ = db.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenantID)
			return err
		})
	})

	slug := "c-" + uuid.NewString()[:8]
	var courseID uuid.UUID
	err = db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO courses (tenant_id, slug, title, status)
			 VALUES ($1, $2, 'The Book of Optics', 'published') RETURNING id`,
			tenantID, slug).Scan(&courseID)
	})
	if err != nil {
		t.Fatalf("seed course: %v", err)
	}

	return fixture{
		db:     db,
		svc:    certify.NewService(db, certify.NewPostgresRepository(), auditor{audit.NewRecorder()}),
		tenant: tenantID,
		course: courseID,
		slug:   slug,
	}
}

func (f fixture) learner(t *testing.T, name string) uuid.UUID {
	t.Helper()

	id := uuid.New()
	err := f.db.WithTenant(t.Context(), f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO users (id, email, password_hash, name) VALUES ($1, $2, 'x', $3)`,
			id, "u-"+id.String()[:8]+"@example.test", name); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'student')`, f.tenant, id)
		return err
	})
	if err != nil {
		t.Fatalf("seed learner: %v", err)
	}
	return id
}

// issue is what `enroll` does, in the transaction that finished the course.
func (f fixture) issue(t *testing.T, userID uuid.UUID) {
	t.Helper()

	err := f.db.WithTenant(t.Context(), f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.IssueIfEarned(ctx, tx, f.tenant, f.course, userID)
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
}

func (f fixture) only(t *testing.T, userID uuid.UUID) certify.Certificate {
	t.Helper()

	mine, err := f.svc.Mine(t.Context(), f.tenant, userID)
	if err != nil {
		t.Fatalf("mine: %v", err)
	}
	if len(mine) != 1 {
		t.Fatalf("%d certificates, want 1", len(mine))
	}
	return mine[0]
}

// A certificate says who, what, and when — in words, with a number.
func TestACertificateIsIssuedAndVerified(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	learner := f.learner(t, "Al-Khwarizmi")
	f.issue(t, learner)

	issued := f.only(t, learner)

	// Who it is for and what it is for are structured fields, copied at issue and
	// frozen — the certificate names them in their own places, so the body does not
	// have to. The default body is a description, and no placeholder survives into
	// it whether or not it happens to use one.
	if issued.LearnerName != "Al-Khwarizmi" || issued.CourseTitle != "The Book of Optics" {
		t.Errorf("issued to %q for %q", issued.LearnerName, issued.CourseTitle)
	}
	if issued.Body == "" {
		t.Error("the certificate has no body")
	}
	if strings.Contains(issued.Body, "{{") {
		t.Errorf("a placeholder survived into the certificate: %q", issued.Body)
	}

	// Anybody holding the number may read it. That is what a certificate is for.
	verified, err := f.svc.Verify(t.Context(), f.tenant, issued.Serial)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verified.Serial != issued.Serial || verified.Revoked() {
		t.Errorf("verified %q, revoked=%v", verified.Serial, verified.Revoked())
	}
}

/*
Issuing twice issues once.

`enroll` calls this in the transaction that finished the course, and a transaction
is retried. A second certificate would give one achievement two numbers, and only
one of them would be the number the learner wrote down.
*/
func TestIssuingTwiceIssuesOnce(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	learner := f.learner(t, "Ibn Sina")

	f.issue(t, learner)
	first := f.only(t, learner)

	f.issue(t, learner)
	second := f.only(t, learner)

	if second.Serial != first.Serial {
		t.Errorf("a second certificate was issued: %q, was %q", second.Serial, first.Serial)
	}
}

/*
The name and the title are copied, never joined.

A person who changes their name has not invalidated the certificate they earned
under the old one, and a course renamed in 2029 did not rename what somebody
finished in 2026.
*/
func TestACertificateRemembersTheNamesItWasIssuedWith(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	learner := f.learner(t, "Maryam al-Astrulabi")
	f.issue(t, learner)

	err := f.db.WithTenant(t.Context(), f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET name = 'Somebody Else' WHERE id = $1`, learner); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE courses SET title = 'Renamed' WHERE tenant_id = $1 AND id = $2`,
			f.tenant, f.course)
		return err
	})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}

	issued := f.only(t, learner)
	if issued.LearnerName != "Maryam al-Astrulabi" {
		t.Errorf("the certificate now says %q", issued.LearnerName)
	}
	if issued.CourseTitle != "The Book of Optics" {
		t.Errorf("the certificate now certifies %q", issued.CourseTitle)
	}
}

/*
A revoked certificate still answers, and says it is revoked.

A code that stopped resolving is indistinguishable from a code that was never
real, which is the answer a forger wants. Somebody is holding this number.
*/
func TestARevokedCertificateStillAnswers(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	learner := f.learner(t, "Al-Biruni")
	f.issue(t, learner)
	issued := f.only(t, learner)

	if err := f.svc.Revoke(t.Context(), f.tenant, issued.Serial, "Plagiarism.", learner); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	verified, err := f.svc.Verify(t.Context(), f.tenant, issued.Serial)
	if err != nil {
		t.Fatalf("a revoked certificate stopped resolving: %v", err)
	}
	if !verified.Revoked() {
		t.Error("the certificate does not say it was withdrawn")
	}
	if verified.RevokedReason != "Plagiarism." {
		t.Errorf("the reason is %q", verified.RevokedReason)
	}

	// And it keeps saying so. Revoking twice is not an error, and not a second event.
	if err := f.svc.Revoke(t.Context(), f.tenant, issued.Serial, "Again.", learner); err != nil {
		t.Errorf("revoking twice: %v", err)
	}
	again, _ := f.svc.Verify(t.Context(), f.tenant, issued.Serial)
	if again.RevokedReason != "Plagiarism." {
		t.Errorf("the second revocation overwrote the first reason: %q", again.RevokedReason)
	}
}

// A serial nobody issued is not found, and does not say why.
func TestAnUnknownSerialIsNotFound(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	if _, err := f.svc.Verify(t.Context(), f.tenant, "CERT-0000-0000-0000-0000"); !errors.Is(err, certify.ErrNotFound) {
		t.Errorf("an invented serial returned %v", err)
	}
	if err := f.svc.Revoke(t.Context(), f.tenant, "CERT-0000", "why", uuid.New()); !errors.Is(err, certify.ErrNotFound) {
		t.Errorf("revoking an invented serial returned %v", err)
	}
}

/*
A certificate belongs to a workspace.

Verification is unauthenticated, and the workspace comes from the request's host.
A serial issued next door must not resolve here — otherwise anybody with a code
from any workspace on the installation could verify it against any other.
*/
func TestACertificateDoesNotVerifyInAnotherWorkspace(t *testing.T) {
	t.Parallel()

	mine := newFixture(t)
	theirs := newFixture(t)

	learner := theirs.learner(t, "Somebody Else")
	theirs.issue(t, learner)
	issued := theirs.only(t, learner)

	if _, err := mine.svc.Verify(t.Context(), mine.tenant, issued.Serial); !errors.Is(err, certify.ErrNotFound) {
		t.Errorf("another workspace's certificate verified here: %v", err)
	}
}

/*
A course prints the template it points at, and the words are frozen at issue.

Editing a template afterwards does not rewrite the certificates already printed
from it — including the ones on somebody's wall.
*/
func TestACoursePrintsItsOwnTemplateAndTheWordsAreFrozen(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	template, err := f.svc.CreateTemplate(t.Context(), f.tenant, certify.Template{
		Name:      "Formal",
		Title:     "Diploma",
		Body:      "{{learner}} has completed {{course}}.",
		Signatory: "The Registrar",
	})
	if err != nil {
		t.Fatalf("create template: %v", err)
	}
	if err := f.svc.SetCourseTemplate(t.Context(), f.tenant, f.slug, &template.ID); err != nil {
		t.Fatalf("set template: %v", err)
	}

	learner := f.learner(t, "Fatima al-Fihri")
	f.issue(t, learner)
	issued := f.only(t, learner)

	if issued.Title != "Diploma" || issued.Signatory != "The Registrar" {
		t.Errorf("printed %q signed by %q", issued.Title, issued.Signatory)
	}
	if issued.Body != "Fatima al-Fihri has completed The Book of Optics." {
		t.Errorf("printed %q", issued.Body)
	}

	// Delete the template. The certificate keeps its words, and the course falls
	// back to the default for whoever finishes it next.
	if err := f.svc.DeleteTemplate(t.Context(), f.tenant, template.ID); err != nil {
		t.Fatalf("delete template: %v", err)
	}

	after := f.only(t, learner)
	if after.Body != issued.Body || after.Title != issued.Title {
		t.Errorf("deleting the template rewrote the certificate: %q / %q", after.Title, after.Body)
	}

	next := f.learner(t, "Somebody Later")
	f.issue(t, next)
	if printed := f.only(t, next); printed.Title != certify.DefaultTemplate().Title {
		t.Errorf("after the template was deleted, the course printed %q", printed.Title)
	}
}

// A course cannot print another workspace's template. The service checks before
// it assigns; RLS is the net, not the check.
func TestACourseCannotPrintAnotherWorkspacesTemplate(t *testing.T) {
	t.Parallel()

	mine := newFixture(t)
	theirs := newFixture(t)

	template, err := theirs.svc.CreateTemplate(t.Context(), theirs.tenant, certify.Template{
		Name: "Theirs", Title: "T", Body: "B",
	})
	if err != nil {
		t.Fatalf("create template: %v", err)
	}

	if err := mine.svc.SetCourseTemplate(t.Context(), mine.tenant, mine.slug, &template.ID); err == nil {
		t.Fatal("a course was pointed at another workspace's certificate template")
	}
}

// Two templates cannot share a name, or an author choosing one from a list is
// choosing between two identical rows.
func TestTemplateNamesAreUniquePerWorkspace(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	template := certify.Template{Name: "Formal", Title: "T", Body: "B"}

	if _, err := f.svc.CreateTemplate(t.Context(), f.tenant, template); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.svc.CreateTemplate(t.Context(), f.tenant, template); !errors.Is(err, certify.ErrTemplateExists) {
		t.Fatalf("a second template took the same name: %v", err)
	}
}

// The built-in default is in the list once, and it is not a row anybody can edit.
func TestTemplatesListsTheDefaultFirst(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	if _, err := f.svc.CreateTemplate(t.Context(), f.tenant, certify.Template{
		Name: "Alpha", Title: "T", Body: "B",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	templates, err := f.svc.Templates(t.Context(), f.tenant)
	if err != nil {
		t.Fatalf("templates: %v", err)
	}
	if len(templates) != 2 {
		t.Fatalf("%d templates, want the default and mine", len(templates))
	}
	if !templates[0].Builtin {
		t.Error("the built-in default is not first")
	}
	if templates[1].Builtin {
		t.Error("a stored template claims to be built in")
	}
}
