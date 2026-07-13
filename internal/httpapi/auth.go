package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/tenant"
)

// UserView is a user as returned to a client. It never carries a password hash,
// and never carries anything about another tenant's membership.
type UserView struct {
	ID            string `json:"id" format:"uuid"`
	Email         string `json:"email" format:"email"`
	Name          string `json:"name"`
	Role          string `json:"role" enum:"owner,admin,instructor,student"`
	EmailVerified bool   `json:"email_verified"`
}

// TokenBody is a freshly minted token pair.
type TokenBody struct {
	AccessToken string `json:"access_token" doc:"Short-lived bearer token. Send as: Authorization: Bearer <token>"`
	TokenType   string `json:"token_type" enum:"Bearer"`
	ExpiresIn   int    `json:"expires_in" doc:"Seconds until the access token expires"`

	// The refresh token is returned in the body rather than set as a cookie
	// because this API serves a browser app, a mobile app, and a WordPress plugin.
	// A cookie only serves the first, and only from a same-site origin.
	RefreshToken string `json:"refresh_token" doc:"Opaque, long-lived. Exchange at /v1/auth/refresh. Rotates on every use."`
}

// AuthOutput is a login or registration response.
type AuthOutput struct {
	// Credentials must never be cached, by a browser or by anything between us.
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		User   UserView  `json:"user"`
		Tokens TokenBody `json:"tokens"`
	}
}

// TokenOutput is a refresh response.
type TokenOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         TokenBody
}

// MeOutput is the authenticated user's profile.
type MeOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		User UserView `json:"user"`
	}
}

// noStore keeps credentials out of every cache. `no-store` is the only directive
// that forbids writing the response to disk at all.
const noStore = "no-store"

func registerAuth(api huma.API, svc *auth.Service) {
	huma.Register(api, huma.Operation{
		OperationID:   "register",
		Method:        http.MethodPost,
		Path:          "/v1/auth/register",
		Summary:       "Create an account on this workspace",
		Description:   "The first member of a workspace becomes its owner; everyone after is a student.",
		Tags:          []string{"Auth"},
		DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Email    string `json:"email" format:"email" maxLength:"320" doc:"Address, case-insensitive"`
			Password string `json:"password" minLength:"12" maxLength:"256" doc:"At least 12 characters. No composition rules."`
			Name     string `json:"name" maxLength:"200"`
		}
	}) (*AuthOutput, error) {
		pair, user, role, err := svc.Register(ctx, tenant.ID(ctx),
			auth.Credentials{Email: in.Body.Email, Password: in.Body.Password},
			in.Body.Name, requestContextFrom(ctx))
		if err != nil {
			return nil, authError(err)
		}

		out := &AuthOutput{CacheControl: noStore}
		out.Body.User = userView(user, role)
		out.Body.Tokens = tokenBody(pair)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "login",
		Method:      http.MethodPost,
		Path:        "/v1/auth/login",
		Summary:     "Exchange credentials for tokens",
		Description: "A missing account, a wrong password, and a suspended membership are indistinguishable, " +
			"by design and in constant time.",
		Tags: []string{"Auth"},
	}, func(ctx context.Context, in *struct {
		Body struct {
			Email    string `json:"email" format:"email" maxLength:"320"`
			Password string `json:"password" maxLength:"256"`
		}
	}) (*AuthOutput, error) {
		pair, user, role, err := svc.Login(ctx, tenant.ID(ctx),
			auth.Credentials{Email: in.Body.Email, Password: in.Body.Password},
			requestContextFrom(ctx))
		if err != nil {
			return nil, authError(err)
		}

		out := &AuthOutput{CacheControl: noStore}
		out.Body.User = userView(user, role)
		out.Body.Tokens = tokenBody(pair)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "refresh-session",
		Method:      http.MethodPost,
		Path:        "/v1/auth/refresh",
		Summary:     "Rotate a refresh token for a new pair",
		Description: "The presented token is invalidated. Presenting it again revokes the entire session " +
			"family, because a token that arrives twice was stolen.",
		Tags: []string{"Auth"},
	}, func(ctx context.Context, in *struct {
		Body struct {
			RefreshToken string `json:"refresh_token" maxLength:"128"`
		}
	}) (*TokenOutput, error) {
		pair, err := svc.Refresh(ctx, tenant.ID(ctx), in.Body.RefreshToken, requestContextFrom(ctx))
		if err != nil {
			return nil, authError(err)
		}
		return &TokenOutput{CacheControl: noStore, Body: tokenBody(pair)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "logout",
		Method:        http.MethodPost,
		Path:          "/v1/auth/logout",
		Summary:       "Revoke the current session",
		Description:   "Other devices stay signed in.",
		Tags:          []string{"Auth"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, _ *struct{}) (*struct{}, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}
		if err := svc.Logout(ctx, p, requestContextFrom(ctx)); err != nil {
			return nil, authError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "request-password-reset",
		Method:      http.MethodPost,
		Path:        "/v1/auth/password/forgot",
		Summary:     "Mail a password reset link",
		Description: "Always succeeds, whether or not the address belongs to a member of this workspace. " +
			"An endpoint that answers \"no such account\" is an enumeration oracle.",
		Tags:          []string{"Auth"},
		DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Email string `json:"email" format:"email" maxLength:"320"`
		}
	}) (*struct{}, error) {
		if err := svc.RequestPasswordReset(ctx, tenant.ID(ctx), in.Body.Email, requestContextFrom(ctx)); err != nil {
			return nil, authError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "reset-password",
		Method:        http.MethodPost,
		Path:          "/v1/auth/password/reset",
		Summary:       "Spend a reset token and set a new password",
		Description:   "Revokes every session in this workspace. The token works once.",
		Tags:          []string{"Auth"},
		DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Token    string `json:"token" maxLength:"128"`
			Password string `json:"password" minLength:"12" maxLength:"256" doc:"At least 12 characters. No composition rules."`
		}
	}) (*struct{}, error) {
		if err := svc.ResetPassword(ctx, tenant.ID(ctx), in.Body.Token, in.Body.Password, requestContextFrom(ctx)); err != nil {
			return nil, authError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "verify-email",
		Method:        http.MethodPost,
		Path:          "/v1/auth/email/verify",
		Summary:       "Confirm an email address",
		Description:   "Exchanges the token from a verification email. The token works once.",
		Tags:          []string{"Auth"},
		DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Token string `json:"token" maxLength:"128"`
		}
	}) (*struct{}, error) {
		if err := svc.VerifyEmail(ctx, tenant.ID(ctx), in.Body.Token, requestContextFrom(ctx)); err != nil {
			return nil, authError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "resend-verification-email",
		Method:        http.MethodPost,
		Path:          "/v1/auth/email/verify/resend",
		Summary:       "Mail a fresh verification link",
		Description:   "A no-op for an address already confirmed. Any earlier link stops working.",
		Tags:          []string{"Auth"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, _ *struct{}) (*struct{}, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}
		if err := svc.ResendVerification(ctx, p, requestContextFrom(ctx)); err != nil {
			return nil, authError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-me",
		Method:      http.MethodGet,
		Path:        "/v1/me",
		Summary:     "The authenticated user's profile",
		Description: "The role is read from the database, not from the token, so a demotion takes effect " +
			"immediately rather than when the token expires.",
		Tags:     []string{"Auth"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, _ *struct{}) (*MeOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		user, role, err := svc.Me(ctx, p)
		if err != nil {
			return nil, authError(err)
		}

		out := &MeOutput{CacheControl: "private, no-store"}
		out.Body.User = userView(user, role)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "rename-me",
		Method:      http.MethodPatch,
		Path:        "/v1/me",
		Summary:     "Change your own name",
		Description: "Your own row, your own name. No permission beyond being signed in.",
		Tags:        []string{"Auth"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Body struct {
			Name string `json:"name" minLength:"1" maxLength:"120"`
		}
	}) (*MeOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		user, err := svc.Rename(ctx, p, in.Body.Name, requestContextFrom(ctx))
		if err != nil {
			return nil, authError(err)
		}

		// The role is not the user's to change, so it is read back rather than assumed.
		_, role, err := svc.Me(ctx, p)
		if err != nil {
			return nil, authError(err)
		}

		out := &MeOutput{CacheControl: "private, no-store"}
		out.Body.User = userView(user, role)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "change-my-password",
		Method:        http.MethodPost,
		Path:          "/v1/me/password",
		Summary:       "Change your own password",
		Description:   "The current password is required, and a token is not enough: a stolen session would otherwise be enough to lock somebody out of their own account for good. Every other session is revoked and can no longer be renewed; access tokens already issued are stateless and expire within fifteen minutes. The browser making this request keeps its own session.",
		Tags:          []string{"Auth"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Body struct {
			CurrentPassword string `json:"current_password" minLength:"1" maxLength:"1000"`
			NewPassword     string `json:"new_password" minLength:"12" maxLength:"1000"`
		}
	}) (*struct{}, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		err = svc.ChangePassword(ctx, p, in.Body.CurrentPassword, in.Body.NewPassword, requestContextFrom(ctx))
		if err != nil {
			return nil, authError(err)
		}
		return nil, nil
	})
}

func userView(u auth.User, role string) UserView {
	return UserView{
		ID:            u.ID.String(),
		Email:         u.Email,
		Name:          u.Name,
		Role:          role,
		EmailVerified: u.EmailVerifiedAt != nil,
	}
}

func tokenBody(p auth.TokenPair) TokenBody {
	return TokenBody{
		AccessToken:  p.AccessToken,
		TokenType:    "Bearer",
		ExpiresIn:    p.ExpiresIn,
		RefreshToken: p.RefreshToken,
	}
}

// authError maps the auth package's sentinels onto status codes.
//
// ErrInvalidCredentials is always 401 with one message, whatever went wrong. Any
// finer distinction is an account-enumeration oracle.
func authError(err error) error {
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		return huma.Error401Unauthorized("Those credentials are not valid.")

	case errors.Is(err, auth.ErrNameInvalid):
		return huma.Error422UnprocessableEntity("A name cannot be empty, and cannot be longer than 120 characters.")

	case errors.Is(err, auth.ErrUnauthenticated):
		// The middleware normally answers this before a handler runs. Mapped anyway:
		// a sentinel with no mapping renders as "an unexpected error occurred",
		// which is a poor way to say "please log in".
		return huma.Error401Unauthorized("Authentication is required.")

	case errors.Is(err, auth.ErrRegistrationClosed):
		// Returned for every address once a workspace is claimed, existing or not,
		// so registration cannot be used to discover which addresses exist.
		return huma.Error403Forbidden("This workspace is invitation-only.")

	case errors.Is(err, auth.ErrEmailTaken):
		return huma.Error409Conflict("That email is already registered. Sign in instead.")

	case errors.Is(err, auth.ErrSessionReused):
		// Deliberately indistinguishable from an expired session. Telling the
		// bearer of a stolen token that we detected the theft tells the thief.
		return huma.Error401Unauthorized("Your session has expired. Sign in again.")

	case errors.Is(err, auth.ErrSessionInvalid):
		return huma.Error401Unauthorized("Your session has expired. Sign in again.")

	case errors.Is(err, auth.ErrForbidden):
		return huma.Error403Forbidden("Your role does not permit this action.")

	case errors.Is(err, auth.ErrTokenInvalid):
		// Not a 401: authenticating would not help, and there is nothing to retry.
		// The one useful instruction is to ask for another link.
		return huma.Error400BadRequest("This link is invalid or has expired. Request a new one.")

	case errors.Is(err, auth.ErrPasswordTooShort), errors.Is(err, auth.ErrPasswordTooLong):
		return huma.Error422UnprocessableEntity(err.Error())

	default:
		return err
	}
}
