package enroll_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/enroll"
	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// uniqueEmail is a fresh address per call, so a rerun does not collide on the
// users_email_key from rows the last run left behind.
func uniqueEmail() string { return "u" + uuid.NewString()[:8] + "@example.test" }

// directory is what cmd/ wires over auth: enroll asks who holds an address, and
// auth's own table answers.
type directory struct{ repo *auth.PostgresRepository }

func (d directory) MembersByEmail(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, emails []string) (map[string]uuid.UUID, error) {
	return d.repo.MembersByEmail(ctx, tx, tenantID, emails)
}

func importer(db *database.DB) *enroll.Service {
	return newService(db).WithDirectory(directory{auth.NewPostgresRepository()})
}

// seedMember makes a member with an address an import can name.
func seedMember(t *testing.T, db *database.DB, tenantID uuid.UUID, email string) uuid.UUID {
	t.Helper()

	id := uuid.New()
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO users (id, email, password_hash, name) VALUES ($1, $2, 'x', 'U')`,
			id, email); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'student')`, tenantID, id)
		return err
	})
	if err != nil {
		t.Fatalf("seed member: %v", err)
	}
	return id
}

func outcomes(results []enroll.ImportResult) map[string]string {
	byEmail := make(map[string]string, len(results))
	for _, r := range results {
		byEmail[r.Email] = r.Outcome
	}
	return byEmail
}

/*
The report is the feature.

"We enrolled 400" is not what the person who pasted the list needs; they need the
one address that was misspelled and the three who were already there.
*/
func TestImportingACohortReportsWhatBecameOfEveryAddress(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := importer(db)
	tenantID := seedTenant(t, db)
	slug, _ := seedCourse(t, db, tenantID, 2, true)

	joining := uniqueEmail()
	already := uniqueEmail()
	seedMember(t, db, tenantID, joining)
	alreadyID := seedMember(t, db, tenantID, already)

	// One of them is on the course before the import runs.
	if _, err := svc.Grant(t.Context(), tenantID, slug, alreadyID, actorFor(seedUser(t, db, tenantID))); err != nil {
		t.Fatal(err)
	}

	nobody := uniqueEmail()
	results, err := svc.Import(t.Context(), tenantID, slug, []string{
		"  " + strings.ToUpper(joining) + " ", // trimmed and lower-cased
		already,
		nobody,  // holds no account here
		joining, // listed twice; reported once
	}, actorFor(seedUser(t, db, tenantID)))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	got := outcomes(results)
	want := map[string]string{
		joining: enroll.OutcomeEnrolled,
		already: enroll.OutcomeAlready,
		nobody:  enroll.OutcomeUnknown,
	}
	if len(got) != len(want) {
		t.Fatalf("the report has %d lines, want %d: %+v", len(got), len(want), got)
	}
	for email, outcome := range want {
		if got[email] != outcome {
			t.Errorf("%s = %q, want %q", email, got[email], outcome)
		}
	}
}

// An address nobody here holds is skipped, not signed up. Otherwise anybody with
// course:write could manufacture members, and joining is by invitation.
func TestImportingCreatesNoAccounts(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := importer(db)
	tenantID := seedTenant(t, db)
	slug, _ := seedCourse(t, db, tenantID, 1, true)

	stranger := uniqueEmail()
	if _, err := svc.Import(t.Context(), tenantID, slug, []string{stranger},
		actorFor(seedUser(t, db, tenantID))); err != nil {
		t.Fatalf("Import: %v", err)
	}

	var exists bool
	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)`, stranger).Scan(&exists)
	})
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("the import minted an account for an address nobody had invited")
	}
}

/*
Three queries for a cohort of any size.

A school moving four hundred learners in from another system is exactly when a
query per row would matter, and exactly when nobody is reading the logs.
*/
func TestImportingACohortIsAFixedNumberOfQueries(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := importer(db)
	tenantID := seedTenant(t, db)
	slug, _ := seedCourse(t, db, tenantID, 2, true)
	actor := actorFor(seedUser(t, db, tenantID))

	counts := map[int]int{}
	for _, size := range []int{2, 12} {
		emails := make([]string, 0, size)
		for i := range size {
			email := fmt.Sprintf("cohort-%d-%s@example.test", i, uuid.NewString()[:8])
			seedMember(t, db, tenantID, email)
			emails = append(emails, email)
		}

		counter := &database.Counter{}
		ctx := database.WithCounter(t.Context(), counter)

		results, err := svc.Import(ctx, tenantID, slug, emails, actor)
		if err != nil {
			t.Fatalf("Import: %v", err)
		}
		if len(results) != size {
			t.Fatalf("results = %d, want %d", len(results), size)
		}
		counts[size] = counter.Count()
	}

	if counts[2] != counts[12] {
		t.Errorf("2 learners took %d queries and 12 took %d — the import grows with the cohort",
			counts[2], counts[12])
	}
}

// A purchase is not relabelled by an import. The label is what says the learner may
// not simply cancel and be left with neither the course nor their money.
func TestImportingSomebodyWhoBoughtTheCourseKeepsThePurchase(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := importer(db)
	tenantID := seedTenant(t, db)
	slug, _ := seedCourse(t, db, tenantID, 2, true)

	email := uniqueEmail()
	buyer := seedMember(t, db, tenantID, email)

	// Enrolled the way a paid webhook enrols somebody.
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		courseID, _, err := enroll.NewPostgresRepository().CourseBySlug(ctx, tx, tenantID, slug)
		if err != nil {
			return err
		}
		return svc.GrantInTx(ctx, tx, tenantID, courseID, buyer, enroll.SourcePurchase)
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Import(t.Context(), tenantID, slug, []string{email},
		actorFor(seedUser(t, db, tenantID))); err != nil {
		t.Fatalf("Import: %v", err)
	}

	var source string
	err = db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT e.source FROM enrolments e
			 JOIN courses c ON c.id = e.course_id
			 WHERE e.tenant_id = $1 AND e.user_id = $2 AND c.slug = $3`,
			tenantID, buyer, slug).Scan(&source)
	})
	if err != nil {
		t.Fatal(err)
	}
	if source != enroll.SourcePurchase {
		t.Errorf("source = %q after an import, want %q — the purchase was relabelled", source, enroll.SourcePurchase)
	}
}

func TestAnEmptyImportIsRefused(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := importer(db)
	tenantID := seedTenant(t, db)
	slug, _ := seedCourse(t, db, tenantID, 1, true)
	actor := actorFor(seedUser(t, db, tenantID))

	for name, list := range map[string][]string{
		"nothing":     {},
		"blank lines": {"", "   ", "\t"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.Import(t.Context(), tenantID, slug, list, actor); !errors.Is(err, enroll.ErrNothingToImport) {
				t.Fatalf("err = %v, want ErrNothingToImport", err)
			}
		})
	}

	// And a list longer than one request may carry.
	tooMany := make([]string, enroll.MaxImport+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("%d@example.test", i)
	}
	if _, err := svc.Import(t.Context(), tenantID, slug, tooMany, actor); !errors.Is(err, enroll.ErrTooManyToImport) {
		t.Fatalf("err = %v, want ErrTooManyToImport", err)
	}
}
