package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/leads"
)

// leadsError maps the domain's sentinels. Neither sentence is the domain's own —
// those are wrapped in a package prefix nobody reads, and this text is rendered
// verbatim to somebody filling in a form.
func leadsError(err error) error {
	switch {
	case errors.Is(err, leads.ErrNotAgreed):
		return huma.Error422UnprocessableEntity(
			"Please agree to the terms before sending your details.")
	case errors.Is(err, leads.ErrInvalidRequest):
		return huma.Error422UnprocessableEntity(
			"Check the form: we need your name, an email address, a phone number, and what you're asking about.")
	default:
		return err
	}
}

// DemoRequestOutput answers with nothing but the fact of it.
//
// The row is deliberately not echoed back. This endpoint is unauthenticated, so
// anything it returns is returned to anyone; there is nothing here the sender did
// not just type, and no id worth handing to a stranger.
type DemoRequestOutput struct {
	// A demo request is private by construction and must never be cached — a
	// shared cache holding somebody's phone number is the whole of the problem.
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Received bool `json:"received" doc:"Always true. Somebody will read it and reply."`
	}
}

func registerLeads(api huma.API, svc *leads.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "requestDemo",
		Method:      http.MethodPost,
		Path:        "/v1/demo-requests",
		Summary:     "Ask to be shown the product",
		Description: "Unauthenticated: the point is that the sender has no workspace yet. " +
			"Every well-formed request is accepted identically — the answer never reveals " +
			"whether an address has asked before. Rate-limited per address.",
		Tags:          []string{"Leads"},
		DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Intent string `json:"intent" enum:"creator,school,madrasa,coaching,agency,nonprofit,other" doc:"Who is asking"`
			Name   string `json:"name" maxLength:"200"`
			Email  string `json:"email" format:"email" maxLength:"320"`
			Phone  string `json:"phone" maxLength:"40" doc:"However they write it. Not validated into a shape."`
			Agreed bool   `json:"agreed" doc:"The terms, ticked. False is refused."`
		}
	}) (*DemoRequestOutput, error) {
		_, err := svc.Request(ctx, leads.NewDemoRequest{
			Intent: in.Body.Intent,
			Name:   in.Body.Name,
			Email:  in.Body.Email,
			Phone:  in.Body.Phone,
			Agreed: in.Body.Agreed,
		})
		if err != nil {
			return nil, leadsError(err)
		}

		out := &DemoRequestOutput{CacheControl: noStore}
		out.Body.Received = true
		return out, nil
	})
}
