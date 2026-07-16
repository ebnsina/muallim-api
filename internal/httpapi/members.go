package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/tenant"
)

// InvitationView is an invitation as returned to a client. It never carries the
// token: that is shown once, to the inviter, at creation.
type InvitationView struct {
	ID        string    `json:"id" format:"uuid"`
	Email     string    `json:"email" format:"email"`
	Role      string    `json:"role" enum:"owner,admin,instructor,student"`
	Status    string    `json:"status" enum:"pending,accepted,revoked,expired"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateInvitationOutput describes the invitation that was mailed.
//
// It deliberately does not carry the token. The link goes to the invited address
// and nowhere else, which is what lets accepting it stand as proof that the
// person holding it controls that inbox. Returning the token here would hand the
// inviter a credential for an address they do not own.
type CreateInvitationOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Invitation InvitationView `json:"invitation"`
	}
}

// ListInvitationsOutput is the workspace's invitations, newest first.
type ListInvitationsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Invitations []InvitationView `json:"invitations"`

		// NextCursor is opaque. Pass it back as `cursor` for the next page; its
		// absence means this was the last.
		NextCursor string `json:"next_cursor,omitempty"`
		HasMore    bool   `json:"has_more"`
	}
}

// MemberView is a member of the workspace.
type MemberView struct {
	UserID        string    `json:"user_id" format:"uuid"`
	Email         string    `json:"email" format:"email"`
	Name          string    `json:"name"`
	Role          string    `json:"role" enum:"owner,admin,instructor,student"`
	Status        string    `json:"status" enum:"active,suspended"`
	EmailVerified bool      `json:"email_verified"`
	JoinedAt      time.Time `json:"joined_at" doc:"When they joined this workspace — not when the account was made."`
}

// ListMembersOutput is the workspace's members.
type ListMembersOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Members []MemberView `json:"members"`

		// NextCursor is opaque. Pass it back as `cursor` for the next page; its
		// absence means this was the last.
		NextCursor string `json:"next_cursor,omitempty"`
		HasMore    bool   `json:"has_more"`
	}
}

func registerMembers(api huma.API, svc *auth.Service) {
	huma.Register(api, huma.Operation{
		OperationID:   "create-invitation",
		Method:        http.MethodPost,
		Path:          "/v1/invitations",
		Summary:       "Invite someone to this workspace",
		Description:   "Requires user:manage. Only an owner may invite an owner. The link is emailed to the invited address and is never returned here.",
		Tags:          []string{"Members"},
		DefaultStatus: http.StatusCreated,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Body struct {
			Email string `json:"email" format:"email" maxLength:"320"`
			Role  string `json:"role" enum:"owner,admin,instructor,student" default:"student"`
		}
	}) (*CreateInvitationOutput, error) {
		p, err := requirePermission(ctx, auth.PermUserManage)
		if err != nil {
			return nil, err
		}

		// The workspace name goes in the email. The domain layer has no way to read
		// it: tenant is a sibling domain package, and auth may not import one.
		workspace, _ := tenant.FromContext(ctx)

		// The token is discarded here, on purpose. It reaches the invitee by email and
		// by no other route, which is what makes the link evidence of who they are.
		inv, _, err := svc.Invite(ctx, p, in.Body.Email, in.Body.Role, workspace.Name, requestContextFrom(ctx))
		if err != nil {
			return nil, membersError(err)
		}

		out := &CreateInvitationOutput{CacheControl: noStore}
		out.Body.Invitation = invitationView(inv)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "accept-invitation",
		Method:      http.MethodPost,
		Path:        "/v1/auth/invitations/accept",
		Summary:     "Join a workspace with an invitation token",
		Description: "If the invited address already has an account on another workspace, its existing " +
			"password must be supplied: the invitation proves the workspace wants the address, " +
			"not that the requester owns it. Otherwise the password creates a new account.",
		Tags: []string{"Auth"},
	}, func(ctx context.Context, in *struct {
		Body struct {
			Token    string `json:"token" maxLength:"128"`
			Password string `json:"password" minLength:"12" maxLength:"256" doc:"The account's existing password, or a new one if the address has no account."`
			Name     string `json:"name,omitempty" maxLength:"200" doc:"Ignored when the address already has an account."`
		}
	}) (*AuthOutput, error) {
		pair, user, role, err := svc.AcceptInvitation(ctx, tenant.ID(ctx),
			in.Body.Token, in.Body.Password, in.Body.Name, requestContextFrom(ctx))
		if err != nil {
			return nil, membersError(err)
		}

		out := &AuthOutput{CacheControl: noStore}
		out.Body.User = userView(user, role)
		out.Body.Tokens = tokenBody(pair)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-invitations",
		Method:      http.MethodGet,
		Path:        "/v1/invitations",
		Summary:     "List this workspace's invitations",
		Tags:        []string{"Members"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor string `query:"cursor" maxLength:"128" doc:"Opaque cursor from a previous page."`
	}) (*ListInvitationsOutput, error) {
		p, err := requirePermission(ctx, auth.PermUserManage)
		if err != nil {
			return nil, err
		}

		page, err := svc.Invitations(ctx, p, auth.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, membersError(err)
		}

		out := &ListInvitationsOutput{CacheControl: "private, no-store"}
		out.Body.Invitations = make([]InvitationView, 0, len(page.Items))
		for _, inv := range page.Items {
			out.Body.Invitations = append(out.Body.Invitations, invitationView(inv))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "revoke-invitation",
		Method:        http.MethodDelete,
		Path:          "/v1/invitations/{id}",
		Summary:       "Withdraw an outstanding invitation",
		Tags:          []string{"Members"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermUserManage)
		if err != nil {
			return nil, err
		}

		id, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity("The invitation id is not a valid UUID.")
		}
		if err := svc.RevokeInvitationByID(ctx, p, id, requestContextFrom(ctx)); err != nil {
			return nil, membersError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-members",
		Method:      http.MethodGet,
		Path:        "/v1/members",
		Summary:     "List this workspace's members",
		Description: "One joined query, not a membership query followed by a user query per row.",
		Tags:        []string{"Members"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor string `query:"cursor" maxLength:"128" doc:"Opaque cursor from a previous page."`
	}) (*ListMembersOutput, error) {
		p, err := requirePermission(ctx, auth.PermUserRead)
		if err != nil {
			return nil, err
		}

		page, err := svc.Members(ctx, p, auth.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, membersError(err)
		}

		out := &ListMembersOutput{CacheControl: "private, no-store"}
		out.Body.Members = make([]MemberView, 0, len(page.Items))
		for _, m := range page.Items {
			out.Body.Members = append(out.Body.Members, MemberView{
				UserID:        m.User.ID.String(),
				Email:         m.User.Email,
				Name:          m.User.Name,
				Role:          m.Role,
				Status:        m.Status,
				EmailVerified: m.User.EmailVerifiedAt != nil,
				JoinedAt:      m.JoinedAt,
			})
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "change-member-role",
		Method:        http.MethodPatch,
		Path:          "/v1/members/{user_id}",
		Summary:       "Promote or demote a member",
		Description:   "Requires user:manage. Nobody may change their own role, and a workspace never loses its last owner. The member's sessions are revoked so the change takes effect immediately.",
		Tags:          []string{"Members"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		UserID string `path:"user_id" format:"uuid"`
		Body   struct {
			Role string `json:"role" enum:"owner,admin,instructor,student"`
		}
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermUserManage)
		if err != nil {
			return nil, err
		}

		userID, err := uuid.Parse(in.UserID)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity("The user id is not a valid UUID.")
		}
		if err := svc.ChangeMemberRole(ctx, p, userID, in.Body.Role, requestContextFrom(ctx)); err != nil {
			return nil, membersError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "remove-member",
		Method:        http.MethodDelete,
		Path:          "/v1/members/{user_id}",
		Summary:       "Remove a member from this workspace",
		Description:   "Their global account survives; only the membership is deleted. A workspace never loses its last owner.",
		Tags:          []string{"Members"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		UserID string `path:"user_id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermUserManage)
		if err != nil {
			return nil, err
		}

		userID, err := uuid.Parse(in.UserID)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity("The user id is not a valid UUID.")
		}
		if err := svc.RemoveMember(ctx, p, userID, requestContextFrom(ctx)); err != nil {
			return nil, membersError(err)
		}
		return nil, nil
	})
}

func invitationView(inv auth.Invitation) InvitationView {
	return InvitationView{
		ID:        inv.ID.String(),
		Email:     inv.Email,
		Role:      inv.Role,
		Status:    inv.Status(time.Now()),
		ExpiresAt: inv.ExpiresAt,
		CreatedAt: inv.CreatedAt,
	}
}

// membersError maps the membership sentinels onto status codes.
func membersError(err error) error {
	switch {
	case errors.Is(err, auth.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page link is no longer valid. Start from the first page.")

	case errors.Is(err, auth.ErrInvitationInvalid):
		return huma.Error404NotFound("That invitation is invalid, expired, or already used.")

	case errors.Is(err, auth.ErrInvalidCredentials):
		// Accepting an invitation for an address that already has an account, with
		// the wrong password for it.
		return huma.Error401Unauthorized("Those credentials are not valid.")

	case errors.Is(err, auth.ErrAlreadyMember):
		return huma.Error409Conflict("That person is already a member of this workspace.")

	case errors.Is(err, auth.ErrInvitationPending):
		return huma.Error409Conflict("An invitation for that address is already outstanding.")

	case errors.Is(err, auth.ErrRegistrationClosed):
		return huma.Error403Forbidden("This workspace is invitation-only.")

	case errors.Is(err, auth.ErrLastOwner):
		return huma.Error409Conflict("A workspace must keep at least one owner.")

	case errors.Is(err, auth.ErrSelfModification):
		return huma.Error409Conflict("You cannot change your own role or remove yourself.")

	case errors.Is(err, auth.ErrNotMember):
		return huma.Error404NotFound("That person is not a member of this workspace.")

	case errors.Is(err, auth.ErrForbidden):
		return huma.Error403Forbidden("Your role does not permit this action.")

	default:
		return authError(err)
	}
}
