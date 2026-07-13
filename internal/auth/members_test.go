package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/auth"
)

// claim bootstraps a workspace and returns its owner's principal.
func claim(t *testing.T, svc *auth.Service, tenantID uuid.UUID) (auth.Principal, string) {
	t.Helper()

	email := uniqueEmail()
	pair, _, _, err := svc.Register(t.Context(), tenantID,
		auth.Credentials{Email: email, Password: password}, "Owner", auth.RequestContext{})
	if err != nil {
		t.Fatalf("claim workspace: %v", err)
	}

	p, err := svc.Verify(pair.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	return p, email
}

func TestInviteAndAcceptCreatesANewAccount(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)
	owner, _ := claim(t, svc, tenantID)

	email := uniqueEmail()
	inv, token, err := svc.Invite(t.Context(), owner, email, auth.RoleInstructor, "Test Workspace", auth.RequestContext{})
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if token == "" {
		t.Fatal("Invite returned no token")
	}
	if inv.Role != auth.RoleInstructor {
		t.Errorf("role = %q, want instructor", inv.Role)
	}

	pair, user, role, err := svc.AcceptInvitation(t.Context(), tenantID, token, password, "Newcomer", auth.RequestContext{})
	if err != nil {
		t.Fatalf("AcceptInvitation: %v", err)
	}
	if user.Email != email {
		t.Errorf("email = %q, want %q", user.Email, email)
	}
	if role != auth.RoleInstructor {
		t.Errorf("role = %q, want the invited role", role)
	}
	if pair.AccessToken == "" {
		t.Error("accepting an invitation did not start a session")
	}

	// And they can now log in normally.
	if _, _, _, err := svc.Login(t.Context(), tenantID,
		auth.Credentials{Email: email, Password: password}, auth.RequestContext{}); err != nil {
		t.Errorf("the invited user cannot log in: %v", err)
	}
}

// The gap this feature exists to close: a person with an account on one workspace
// can now join a second.
func TestInvitationLetsAnExistingAccountJoinASecondWorkspace(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	acme := seedTenant(t, db)
	globex := seedTenant(t, db)

	// Ada owns acme.
	_, adaEmail := claim(t, svc, acme)

	// Globex is claimed by somebody else, then invites Ada.
	globexOwner, _ := claim(t, svc, globex)
	_, token, err := svc.Invite(t.Context(), globexOwner, adaEmail, auth.RoleStudent, "Test Workspace", auth.RequestContext{})
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}

	_, user, role, err := svc.AcceptInvitation(t.Context(), globex, token, password, "", auth.RequestContext{})
	if err != nil {
		t.Fatalf("AcceptInvitation: %v", err)
	}
	if role != auth.RoleStudent {
		t.Errorf("role on globex = %q, want student", role)
	}

	// One account, two workspaces, two roles.
	if _, _, ownerRole, err := svc.Login(t.Context(), acme,
		auth.Credentials{Email: adaEmail, Password: password}, auth.RequestContext{}); err != nil {
		t.Fatalf("login on acme: %v", err)
	} else if ownerRole != auth.RoleOwner {
		t.Errorf("role on acme = %q, want owner", ownerRole)
	}

	// One account, seen from both workspaces. Counted under a binding, because the
	// users SELECT policy hides a row from anyone with no membership to it — an
	// unbound query would see zero and prove nothing.
	for name, tid := range map[string]uuid.UUID{"acme": acme, "globex": globex} {
		var accounts int
		err = db.WithTenantReadOnly(t.Context(), tid, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE id = $1`, user.ID).Scan(&accounts)
		})
		if err != nil {
			t.Fatal(err)
		}
		if accounts != 1 {
			t.Errorf("%s sees %d accounts for the invitee, want 1 — a user is global", name, accounts)
		}
	}
}

// The security property. An invitation proves the workspace wants that address.
// It does not prove the person holding the link owns it.
func TestAcceptingWithAnExistingAccountRequiresItsPassword(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	acme := seedTenant(t, db)
	globex := seedTenant(t, db)

	_, adaEmail := claim(t, svc, acme)
	globexOwner, _ := claim(t, svc, globex)

	_, token, err := svc.Invite(t.Context(), globexOwner, adaEmail, auth.RoleAdmin, "Test Workspace", auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}

	// An attacker who intercepts the link cannot take over Ada's account by
	// choosing a new password for it.
	_, _, _, err = svc.AcceptInvitation(t.Context(), globex, token,
		"a password the attacker just made up", "Mallory", auth.RequestContext{})
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials — the invitation was an account takeover", err)
	}

	// The invitation is still pending, so Ada can still accept it herself.
	if _, _, role, err := svc.AcceptInvitation(t.Context(), globex, token, password, "", auth.RequestContext{}); err != nil {
		t.Errorf("the real owner could not accept after a failed attempt: %v", err)
	} else if role != auth.RoleAdmin {
		t.Errorf("role = %q, want admin", role)
	}
}

func TestInvitationIsSingleUse(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)
	owner, _ := claim(t, svc, tenantID)

	_, token, err := svc.Invite(t.Context(), owner, uniqueEmail(), auth.RoleStudent, "Test Workspace", auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := svc.AcceptInvitation(t.Context(), tenantID, token, password, "A", auth.RequestContext{}); err != nil {
		t.Fatal(err)
	}

	_, _, _, err = svc.AcceptInvitation(t.Context(), tenantID, token, password, "B", auth.RequestContext{})
	if !errors.Is(err, auth.ErrInvitationInvalid) {
		t.Errorf("err = %v, want ErrInvitationInvalid — an invitation must be single-use", err)
	}
}

func TestRevokedInvitationCannotBeAccepted(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)
	owner, _ := claim(t, svc, tenantID)

	inv, token, err := svc.Invite(t.Context(), owner, uniqueEmail(), auth.RoleStudent, "Test Workspace", auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RevokeInvitationByID(t.Context(), owner, inv.ID, auth.RequestContext{}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if _, _, _, err := svc.AcceptInvitation(t.Context(), tenantID, token, password, "A", auth.RequestContext{}); !errors.Is(err, auth.ErrInvitationInvalid) {
		t.Errorf("err = %v, want ErrInvitationInvalid", err)
	}
}

func TestInvitationTokensAreNotGuessableOrCrossTenant(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	acme := seedTenant(t, db)
	globex := seedTenant(t, db)

	owner, _ := claim(t, svc, acme)
	_ = mustClaim(t, svc, globex)

	_, token, err := svc.Invite(t.Context(), owner, uniqueEmail(), auth.RoleStudent, "Test Workspace", auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}

	// A valid acme token, presented to globex.
	if _, _, _, err := svc.AcceptInvitation(t.Context(), globex, token, password, "X", auth.RequestContext{}); !errors.Is(err, auth.ErrInvitationInvalid) {
		t.Errorf("err = %v; an invitation to one workspace was accepted by another", err)
	}

	// Garbage.
	if _, _, _, err := svc.AcceptInvitation(t.Context(), acme, "not-a-token", password, "X", auth.RequestContext{}); !errors.Is(err, auth.ErrInvitationInvalid) {
		t.Errorf("err = %v, want ErrInvitationInvalid", err)
	}
}

func mustClaim(t *testing.T, svc *auth.Service, tenantID uuid.UUID) auth.Principal {
	t.Helper()
	p, _ := claim(t, svc, tenantID)
	return p
}

func TestOnlyAnOwnerMayInviteAnOwner(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)
	owner, _ := claim(t, svc, tenantID)

	// Promote somebody to admin.
	_, token, err := svc.Invite(t.Context(), owner, uniqueEmail(), auth.RoleAdmin, "Test Workspace", auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	pair, _, _, err := svc.AcceptInvitation(t.Context(), tenantID, token, password, "Admin", auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := svc.Verify(pair.AccessToken)
	if err != nil {
		t.Fatal(err)
	}

	// An admin inviting an owner would be promoting themselves via an alias.
	if _, _, err := svc.Invite(t.Context(), admin, uniqueEmail(), auth.RoleOwner, "Test Workspace", auth.RequestContext{}); !errors.Is(err, auth.ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden — an admin minted an owner", err)
	}

	if _, _, err := svc.Invite(t.Context(), owner, uniqueEmail(), auth.RoleOwner, "Test Workspace", auth.RequestContext{}); err != nil {
		t.Errorf("an owner could not invite an owner: %v", err)
	}
}

func TestDuplicatePendingInvitationIsRejected(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)
	owner, _ := claim(t, svc, tenantID)

	email := uniqueEmail()
	if _, _, err := svc.Invite(t.Context(), owner, email, auth.RoleStudent, "Test Workspace", auth.RequestContext{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Invite(t.Context(), owner, email, auth.RoleStudent, "Test Workspace", auth.RequestContext{}); !errors.Is(err, auth.ErrInvitationPending) {
		t.Errorf("err = %v, want ErrInvitationPending", err)
	}
}

func TestChangeMemberRoleGuards(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)
	owner, _ := claim(t, svc, tenantID)

	_, token, err := svc.Invite(t.Context(), owner, uniqueEmail(), auth.RoleStudent, "Test Workspace", auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	pair, student, _, err := svc.AcceptInvitation(t.Context(), tenantID, token, password, "S", auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("nobody edits their own role", func(t *testing.T) {
		if err := svc.ChangeMemberRole(t.Context(), owner, owner.UserID, auth.RoleStudent, auth.RequestContext{}); !errors.Is(err, auth.ErrSelfModification) {
			t.Errorf("err = %v, want ErrSelfModification", err)
		}
	})

	t.Run("a workspace keeps its last owner", func(t *testing.T) {
		// Demoting the only owner would leave nobody able to administer it.
		second, _ := claim(t, svc, seedTenant(t, db))
		if err := svc.ChangeMemberRole(t.Context(), second, second.UserID, auth.RoleAdmin, auth.RequestContext{}); !errors.Is(err, auth.ErrSelfModification) {
			t.Errorf("err = %v, want ErrSelfModification", err)
		}
	})

	t.Run("promotion revokes the member's sessions", func(t *testing.T) {
		if err := svc.ChangeMemberRole(t.Context(), owner, student.ID, auth.RoleInstructor, auth.RequestContext{}); err != nil {
			t.Fatalf("ChangeMemberRole: %v", err)
		}
		// Their refresh token is dead, so the stale role in their access token
		// cannot outlive the change.
		if _, err := svc.Refresh(t.Context(), tenantID, pair.RefreshToken, auth.RequestContext{}); err == nil {
			t.Error("a role change left the member's sessions alive; their old role survives until the token expires")
		}
	})
}

// Removing somebody from a workspace is not erasing them from the platform.
func TestRemoveMemberKeepsTheGlobalAccount(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	acme := seedTenant(t, db)
	globex := seedTenant(t, db)

	acmeOwner, _ := claim(t, svc, acme)
	globexOwner, _ := claim(t, svc, globex)

	// The student belongs to both workspaces.
	studentEmail := uniqueEmail()
	_, token, err := svc.Invite(t.Context(), acmeOwner, studentEmail, auth.RoleStudent, "Test Workspace", auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	_, student, _, err := svc.AcceptInvitation(t.Context(), acme, token, password, "S", auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}

	_, token2, err := svc.Invite(t.Context(), globexOwner, studentEmail, auth.RoleStudent, "Test Workspace", auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := svc.AcceptInvitation(t.Context(), globex, token2, password, "", auth.RequestContext{}); err != nil {
		t.Fatal(err)
	}

	if err := svc.RemoveMember(t.Context(), acmeOwner, student.ID, auth.RequestContext{}); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}

	// Gone from acme.
	if _, _, _, err := svc.Login(t.Context(), acme,
		auth.Credentials{Email: studentEmail, Password: password}, auth.RequestContext{}); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("a removed member can still sign in: %v", err)
	}

	// Still themselves on globex: the account is global, only the membership went.
	if _, _, role, err := svc.Login(t.Context(), globex,
		auth.Credentials{Email: studentEmail, Password: password}, auth.RequestContext{}); err != nil {
		t.Errorf("removing a member from one workspace signed them out of another: %v", err)
	} else if role != auth.RoleStudent {
		t.Errorf("role on globex = %q, want student", role)
	}
}

func TestRemoveLastOwnerIsRefused(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)
	owner, _ := claim(t, svc, tenantID)

	// Bring in a second owner so the first can act on them.
	_, token, err := svc.Invite(t.Context(), owner, uniqueEmail(), auth.RoleOwner, "Test Workspace", auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	_, second, _, err := svc.AcceptInvitation(t.Context(), tenantID, token, password, "Two", auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}

	// Two owners: removing one is fine.
	if err := svc.RemoveMember(t.Context(), owner, second.ID, auth.RequestContext{}); err != nil {
		t.Fatalf("removing one of two owners: %v", err)
	}

	// One owner left. Demoting them via another owner is impossible — there is no
	// other owner — so exercise the guard through the repository invariant: a
	// second owner is re-invited, then the first tries to demote the second... and
	// the interesting case is the last owner demoting themselves, which the
	// self-modification guard already refuses. So instead: promote a student to
	// owner, then have them demote the original.
	_, token2, err := svc.Invite(t.Context(), owner, uniqueEmail(), auth.RoleOwner, "Test Workspace", auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	pair3, third, _, err := svc.AcceptInvitation(t.Context(), tenantID, token2, password, "Three", auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	thirdPrincipal, err := svc.Verify(pair3.AccessToken)
	if err != nil {
		t.Fatal(err)
	}

	// Third demotes the original owner: allowed, two owners exist.
	if err := svc.ChangeMemberRole(t.Context(), thirdPrincipal, owner.UserID, auth.RoleAdmin, auth.RequestContext{}); err != nil {
		t.Fatalf("demoting one of two owners: %v", err)
	}

	// Now `third` is the only owner. An admin cannot remove them (no user:manage
	// on owner is not a rule, but the last-owner guard is).
	if err := svc.RemoveMember(t.Context(), owner, third.ID, auth.RequestContext{}); !errors.Is(err, auth.ErrLastOwner) {
		t.Errorf("err = %v, want ErrLastOwner — the workspace was left without an owner", err)
	}
}

func TestListMembersAndInvitations(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)
	owner, ownerEmail := claim(t, svc, tenantID)

	if _, _, err := svc.Invite(t.Context(), owner, uniqueEmail(), auth.RoleStudent, "Test Workspace", auth.RequestContext{}); err != nil {
		t.Fatal(err)
	}

	page, err := svc.Members(t.Context(), owner, auth.PageParams{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	members := page.Items
	if len(members) != 1 || members[0].User.Email != ownerEmail || members[0].Role != auth.RoleOwner {
		t.Errorf("members = %+v, want exactly the owner", members)
	}
	if page.HasMore || page.NextCursor != "" {
		t.Error("one member is not more than a page")
	}

	invited, err := svc.Invitations(t.Context(), owner, auth.PageParams{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	invitations := invited.Items
	if len(invitations) != 1 {
		t.Fatalf("invitations = %d, want 1", len(invitations))
	}
	if got := invitations[0].Status(invitations[0].CreatedAt); got != "pending" {
		t.Errorf("status = %q, want pending", got)
	}
}

/*
The page that could not be turned.

`/v1/members` capped at two hundred and offered no cursor, so a school with more
members than that could not be read to the end. The list stopped, and said nothing
about stopping — which is the worst way for a list to be wrong.

This walks a workspace larger than a page and insists on what a keyset promises:
nobody listed twice, and nobody missed.
*/
func TestEveryMemberCanBeReachedByTurningPages(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)
	owner, ownerEmail := claim(t, svc, tenantID)

	// Five more people who actually joined: an invitation is not a membership until
	// somebody accepts it.
	const joiners = 5
	for range joiners {
		email := uniqueEmail()
		_, token, err := svc.Invite(t.Context(), owner, email, auth.RoleStudent,
			"Test Workspace", auth.RequestContext{})
		if err != nil {
			t.Fatalf("Invite: %v", err)
		}
		if _, _, _, err := svc.AcceptInvitation(t.Context(), tenantID, token, password,
			"Joiner", auth.RequestContext{}); err != nil {
			t.Fatalf("AcceptInvitation: %v", err)
		}
	}

	seen := map[string]int{}
	cursor := ""

	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("the cursor never ran out: a page that always has more is a page that never turns")
		}

		page, err := svc.Members(t.Context(), owner, auth.PageParams{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("Members: %v", err)
		}
		for _, m := range page.Items {
			seen[m.User.Email]++
		}

		if !page.HasMore {
			if page.NextCursor != "" {
				t.Error("the last page still offered a cursor")
			}
			break
		}
		if page.NextCursor == "" {
			t.Fatal("there is more, and no way to ask for it")
		}
		cursor = page.NextCursor
	}

	if len(seen) != joiners+1 {
		t.Errorf("paging found %d members, want %d — somebody was skipped", len(seen), joiners+1)
	}
	if seen[ownerEmail] != 1 {
		t.Errorf("the owner was seen %d times, want exactly once", seen[ownerEmail])
	}
	for email, count := range seen {
		if count != 1 {
			t.Errorf("%s appeared %d times across the pages", email, count)
		}
	}
}

// A cursor this API did not issue is refused, rather than quietly serving page one.
func TestAMadeUpCursorIsRefused(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)
	owner, _ := claim(t, svc, tenantID)

	for _, token := range []string{"not-base64!", "bm90LWEtY3Vyc29y"} {
		_, err := svc.Members(t.Context(), owner, auth.PageParams{Cursor: token})
		if !errors.Is(err, auth.ErrInvalidPage) {
			t.Errorf("cursor %q returned %v, want ErrInvalidPage", token, err)
		}
	}
}

/*
An admin may manage members, but not the owners above them.

Creating an owner was already guarded; demoting or removing one was not, so an
admin could strip every co-owner down to the last and leave the workspace with the
one owner they chose. This is the mirror of the create guard, in both directions.
*/
func TestAnAdminCannotUnseatAnOwner(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(t, db)
	tenantID := seedTenant(t, db)
	owner, _ := claim(t, svc, tenantID)

	// A second owner, so the last-owner guard is not what refuses the admin — the
	// rank guard is.
	_, ownerToken, err := svc.Invite(t.Context(), owner, uniqueEmail(), auth.RoleOwner, "Test Workspace", auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	pair2, _, _, err := svc.AcceptInvitation(t.Context(), tenantID, ownerToken, password, "Owner Two", auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	secondOwner, err := svc.Verify(pair2.AccessToken)
	if err != nil {
		t.Fatal(err)
	}

	// And an admin.
	_, adminToken, err := svc.Invite(t.Context(), owner, uniqueEmail(), auth.RoleAdmin, "Test Workspace", auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	pair3, _, _, err := svc.AcceptInvitation(t.Context(), tenantID, adminToken, password, "Admin", auth.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := svc.Verify(pair3.AccessToken)
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.ChangeMemberRole(t.Context(), admin, secondOwner.UserID, auth.RoleStudent, auth.RequestContext{}); !errors.Is(err, auth.ErrForbidden) {
		t.Errorf("demote an owner returned %v, want ErrForbidden", err)
	}
	if err := svc.RemoveMember(t.Context(), admin, secondOwner.UserID, auth.RequestContext{}); !errors.Is(err, auth.ErrForbidden) {
		t.Errorf("remove an owner returned %v, want ErrForbidden", err)
	}

	// An owner still may — the guard is about rank, not about owners being untouchable.
	if err := svc.ChangeMemberRole(t.Context(), owner, secondOwner.UserID, auth.RoleAdmin, auth.RequestContext{}); err != nil {
		t.Errorf("an owner could not demote a co-owner: %v", err)
	}
}
