package auth_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/auth"
	"github.com/ebnsina/lms-api/internal/platform/database"
)

func newMaintenance(t *testing.T, db *database.DB) *auth.Maintenance {
	t.Helper()

	m, err := auth.NewMaintenance(db, auth.NewPostgresRepository(), discardLogger())
	if err != nil {
		t.Fatalf("NewMaintenance: %v", err)
	}
	return m
}

// userExists asks without a tenant bound, which is the only vantage from which
// an orphan is visible at all.
func userExists(t *testing.T, db *database.DB, id uuid.UUID) bool {
	t.Helper()

	var count int
	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE id = $1`, id).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count user: %v", err)
	}
	return count > 0
}

// memberExists checks from inside the workspace, where a member is visible and
// an orphan is not.
func memberExists(t *testing.T, db *database.DB, tenantID, userID uuid.UUID) bool {
	t.Helper()

	var count int
	err := db.WithTenantReadOnly(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE id = $1`, userID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count member: %v", err)
	}
	return count > 0
}

// orphan registers an owner, then strips their membership, which is what
// deleting a workspace or removing its last member amounts to.
func orphan(t *testing.T, db *database.DB, svc *auth.Service, tenantID uuid.UUID) uuid.UUID {
	t.Helper()

	pair, user, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: uniqueEmail(), Password: password}, "Ada", auth.RequestContext{})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := svc.Verify(pair.AccessToken); err != nil {
		t.Fatalf("verify: %v", err)
	}

	err = db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM memberships WHERE user_id = $1`, user.ID)
		return err
	})
	if err != nil {
		t.Fatalf("strip membership: %v", err)
	}
	return user.ID
}

// These tests do not run in parallel with each other.
//
// EraseOrphanedUsers sweeps the whole database — that is what makes it able to
// reach a user no tenant can see — so two sweeps running at once, or a sweep
// running beside a test that has just created an orphan, erase each other's
// fixtures. Serial tests execute while parallel ones are paused, which is the
// isolation this needs and the reason none of the tests below call t.Parallel.

// The gap this closes. A user with no membership anywhere was unreachable from
// any tenant-scoped request, so nobody could erase them — and they are exactly
// the people who ask to be forgotten.
func TestEraseOrphanedUsersErasesAUserWithNoMembership(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)

	userID := orphan(t, db, svc, tenantID)
	if !userExists(t, db, userID) {
		t.Fatal("the fixture did not create a user")
	}

	if _, err := newMaintenance(t, db).EraseOrphanedUsers(t.Context(), 1000); err != nil {
		t.Fatalf("erase: %v", err)
	}

	if userExists(t, db, userID) {
		t.Error("an orphaned user survived erasure")
	}
}

// The invariant that makes this safe to run unattended. A user who still belongs
// to a workspace survives a sweep, because an unbound session cannot see them —
// not because this code remembered to skip them.
func TestEraseOrphanedUsersSparesAUserWhoStillBelongsSomewhere(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)

	_, member, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: uniqueEmail(), Password: password}, "Grace", auth.RequestContext{})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if _, err := newMaintenance(t, db).EraseOrphanedUsers(t.Context(), 1000); err != nil {
		t.Fatalf("erase: %v", err)
	}

	if !memberExists(t, db, tenantID, member.ID) {
		t.Error("a user who still belongs to a workspace was erased")
	}
}

// The mechanism, asserted on its own.
//
// An unbound session sees orphans and nothing else. This is what makes erasure
// safe: Postgres applies SELECT policies to the rows a DELETE has to read, so a
// member is not merely undeletable, they are invisible. Every other guarantee in
// this file rests on this one.
func TestUnboundSessionSeesOnlyOrphans(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)

	orphanID := orphan(t, db, svc, tenantID)

	_, member, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: uniqueEmail(), Password: password}, "Grace", auth.RequestContext{})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if !userExists(t, db, orphanID) {
		t.Error("an unbound session cannot see an orphan, so nobody can ever erase one")
	}
	if userExists(t, db, member.ID) {
		t.Error("an unbound session can see a user who belongs to a workspace")
	}
}

// The observable promise: erasing a user who still belongs somewhere removes
// nothing. The database refuses, rather than this code remembering to filter —
// a policy that trusted the application would stop protecting anything the first
// time a caller forgot.
//
// Which policy refuses is not this test's business, and it cannot tell: with
// `users_orphan_visible` dropped the member is invisible for a different reason,
// and the test still passes. TestUnboundSessionSeesOnlyOrphans pins the
// mechanism; this pins the behaviour.
func TestDeletingAMemberIsRefusedByThePolicy(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)

	_, member, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: uniqueEmail(), Password: password}, "Grace", auth.RequestContext{})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	repo := auth.NewPostgresRepository()

	var deleted bool
	err = db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		deleted, err = repo.DeleteUser(ctx, tx, member.ID)
		return err
	})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted {
		t.Error("row-level security allowed erasing a user who belongs to a workspace")
	}
}

// Erasing takes their sessions, enrolments, and credential tokens with it, and
// leaves the audit trail standing with no actor. A record of what happened must
// outlive the person it happened to; a record naming them must not.
func TestErasureCascadesButKeepsTheAuditTrail(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)

	userID := orphan(t, db, svc, tenantID)

	// Counted from inside the workspace. sessions and audit_log are tenant-scoped,
	// so an unbound session sees neither — which is the whole reason the orphan
	// above needed a policy of its own, and is easy to forget when writing the
	// test that proves it.
	counts := func(t *testing.T) (sessions, auditRows, auditWithActor int) {
		t.Helper()

		err := db.WithTenantReadOnly(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id = $1`, userID).Scan(&sessions); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&auditRows); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE actor_id = $1`, userID).Scan(&auditWithActor)
		})
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		return sessions, auditRows, auditWithActor
	}

	sessionsBefore, _, auditBefore := counts(t)
	if sessionsBefore == 0 || auditBefore == 0 {
		t.Fatalf("fixture is uninteresting: sessions=%d audit entries naming them=%d", sessionsBefore, auditBefore)
	}

	if _, err := newMaintenance(t, db).EraseOrphanedUsers(t.Context(), 1000); err != nil {
		t.Fatalf("erase: %v", err)
	}

	sessionsAfter, auditRows, auditWithActor := counts(t)

	if sessionsAfter != 0 {
		t.Errorf("sessions survived erasure: %d", sessionsAfter)
	}
	if auditRows == 0 {
		t.Error("the audit trail was erased along with the user")
	}
	if auditWithActor != 0 {
		t.Errorf("%d audit entries still name the erased user", auditWithActor)
	}
}

// A sweep with nothing to do does nothing, and a retried job finds the
// survivors. Jobs are retried, so a job must be safe to repeat.
func TestEraseOrphanedUsersIsIdempotent(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)

	userID := orphan(t, db, svc, tenantID)
	maintenance := newMaintenance(t, db)

	if _, err := maintenance.EraseOrphanedUsers(t.Context(), 1000); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if userExists(t, db, userID) {
		t.Fatal("the first sweep did not erase the orphan")
	}

	// The second sweep must not fail, whatever else the database happens to hold.
	if _, err := maintenance.EraseOrphanedUsers(t.Context(), 1000); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
}

func TestNewMaintenanceRefusesMissingDependencies(t *testing.T) {
	if _, err := auth.NewMaintenance(nil, auth.NewPostgresRepository(), discardLogger()); err == nil {
		t.Error("NewMaintenance accepted a nil database")
	}
	if _, err := auth.NewMaintenance(&database.DB{}, nil, discardLogger()); err == nil {
		t.Error("NewMaintenance accepted a nil repository")
	}
}
