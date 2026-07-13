package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/commerce"
)

// MoneyView is an amount and its currency. Minor units, always: a client that wants
// "৳1,200.00" formats it, and a client that wants to add two of them can.
type MoneyView struct {
	AmountMinor int64  `json:"amount_minor" doc:"In the currency's smallest unit. 120000 BDT is 1,200 taka."`
	Currency    string `json:"currency" minLength:"3" maxLength:"3"`
}

// AccountView is a workspace's payment account, as its authors see it.
type AccountView struct {
	Gateway string `json:"gateway" enum:"stripe,fake"`
	Status  string `json:"status" enum:"pending,active,restricted"`

	// ChargesEnabled is the gateway's own answer, not ours. An account can be
	// onboarded and still be refused charges.
	ChargesEnabled bool `json:"charges_enabled"`

	// Ready is the only question a page actually asks: may this workspace sell?
	Ready bool `json:"ready"`
}

// OrderView is one order, as the learner who placed it sees it.
type OrderView struct {
	ID       string    `json:"id" format:"uuid"`
	CourseID string    `json:"course_id" format:"uuid"`
	Price    MoneyView `json:"price"`
	Status   string    `json:"status" enum:"pending,paid,failed,refunded"`
	Gateway  string    `json:"gateway"`

	PaidAt    *time.Time `json:"paid_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

type AccountOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Account AccountView `json:"account"`
	}
}

type OnboardingOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		// URL is the gateway's own onboarding page. Send the author there.
		URL string `json:"url" format:"uri"`
	}
}

type CheckoutOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Order OrderView `json:"order"`

		// URL is the gateway's hosted checkout. A card number never touches this API.
		URL string `json:"url" format:"uri"`
	}
}

type ReceiptsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Orders []OrderView `json:"orders"`
	}
}

// billingCacheControl: what a workspace's account says changes when a gateway
// decides it does, so nothing here is shared and nothing is stored.
const billingCacheControl = "private, no-store"

/*
selling reports whether this deployment can take money at all.

A deployment with no gateway configured has no commerce service, and every one of
these endpoints answers 503 rather than dereferencing nothing. The endpoints are
still in the spec, because a client that cannot see them cannot tell the difference
between "not sold here" and "not built".
*/
func selling(svc *commerce.Service) error {
	if svc == nil {
		return huma.Error503ServiceUnavailable("This deployment takes no payments.")
	}
	return nil
}

func registerCommerce(api huma.API, svc *commerce.Service) {
	huma.Register(api, huma.Operation{
		OperationID:   "connect-payments",
		Method:        http.MethodPost,
		Path:          "/v1/billing/connect",
		Summary:       "Connect this workspace's payment account",
		Description:   "Requires course:write. Returns where to send the author to finish onboarding with the gateway. The workspace is the merchant: it is paid directly, and it owns its own tax, refunds and disputes.",
		Tags:          []string{"Billing"},
		DefaultStatus: http.StatusCreated,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Body struct {
			Gateway   string `json:"gateway" enum:"stripe,fake"`
			ReturnURL string `json:"return_url" format:"uri" maxLength:"2000" doc:"Where the gateway sends the author when onboarding ends."`
		}
	}) (*OnboardingOutput, error) {
		if err := selling(svc); err != nil {
			return nil, err
		}

		p, author, err := authorFor(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		url, err := svc.Connect(ctx, p.TenantID, in.Body.Gateway, in.Body.ReturnURL,
			commerce.Actor{UserID: author.UserID, IP: author.IP, UserAgent: author.UserAgent})
		if err != nil {
			return nil, commerceError(err)
		}

		out := &OnboardingOutput{CacheControl: billingCacheControl}
		out.Body.URL = url
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-payment-account",
		Method:      http.MethodGet,
		Path:        "/v1/billing/account",
		Summary:     "Whether this workspace can be paid",
		Description: "Requires course:write. Asks the gateway, rather than trusting our copy: an account can be onboarded and still be refused charges.",
		Tags:        []string{"Billing"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Gateway string `query:"gateway" enum:"stripe,fake" default:"fake"`
	}) (*AccountOutput, error) {
		if err := selling(svc); err != nil {
			return nil, err
		}

		p, _, err := authorFor(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		account, err := svc.AccountOf(ctx, p.TenantID, in.Gateway)
		if err != nil {
			return nil, commerceError(err)
		}

		out := &AccountOutput{CacheControl: billingCacheControl}
		out.Body.Account = AccountView{
			Gateway:        account.Gateway,
			Status:         account.Status,
			ChargesEnabled: account.ChargesEnabled,
			Ready:          account.Ready(),
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "set-course-price",
		Method:      http.MethodPut,
		Path:        "/v1/courses/{slug}/price",
		Summary:     "Price a course",
		Description: "Requires course:write, and a payment account the gateway will take charges on. A course with no price is free, which is what every course is until this is called.",
		Tags:        []string{"Billing"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
		Body struct {
			AmountMinor int64  `json:"amount_minor" minimum:"1" doc:"In the currency's smallest unit."`
			Currency    string `json:"currency" minLength:"3" maxLength:"3"`
			Gateway     string `json:"gateway" enum:"stripe,fake"`
		}
	}) (*struct{}, error) {
		if err := selling(svc); err != nil {
			return nil, err
		}

		p, author, err := authorFor(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		err = svc.SetPrice(ctx, p.TenantID, in.Slug,
			commerce.Money{AmountMinor: in.Body.AmountMinor, Currency: in.Body.Currency},
			in.Body.Gateway,
			commerce.Actor{UserID: author.UserID, IP: author.IP, UserAgent: author.UserAgent})
		if err != nil {
			return nil, commerceError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "clear-course-price",
		Method:        http.MethodDelete,
		Path:          "/v1/courses/{slug}/price",
		Summary:       "Make a course free",
		Description:   "Requires course:write. Nobody who already bought it loses anything: their enrolment is a row, not a subscription.",
		Tags:          []string{"Billing"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
	}) (*struct{}, error) {
		if err := selling(svc); err != nil {
			return nil, err
		}

		p, author, err := authorFor(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		err = svc.ClearPrice(ctx, p.TenantID, in.Slug,
			commerce.Actor{UserID: author.UserID, IP: author.IP, UserAgent: author.UserAgent})
		if err != nil {
			return nil, commerceError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "buy-course",
		Method:        http.MethodPost,
		Path:          "/v1/courses/{slug}/checkout",
		Summary:       "Buy a course",
		Description:   "Opens a checkout on the workspace's own gateway account and returns where to send the learner. The order is pending until the gateway's webhook says otherwise — the redirect back is not the truth, and nothing is granted on it.",
		Tags:          []string{"Billing"},
		DefaultStatus: http.StatusCreated,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
		Body struct {
			Gateway    string `json:"gateway" enum:"stripe,fake"`
			SuccessURL string `json:"success_url" format:"uri" maxLength:"2000"`
			CancelURL  string `json:"cancel_url" format:"uri" maxLength:"2000"`
		}
	}) (*CheckoutOutput, error) {
		if err := selling(svc); err != nil {
			return nil, err
		}

		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		checkout, err := svc.Buy(ctx, p.TenantID, in.Slug, p.UserID, in.Body.Gateway,
			commerce.CheckoutURLs{Success: in.Body.SuccessURL, Cancel: in.Body.CancelURL},
			commerce.Actor{UserID: p.UserID})
		if err != nil {
			return nil, commerceError(err)
		}

		out := &CheckoutOutput{CacheControl: billingCacheControl}
		out.Body.Order = orderView(checkout.Order)
		out.Body.URL = checkout.URL
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-my-orders",
		Method:      http.MethodGet,
		Path:        "/v1/me/orders",
		Summary:     "What I have bought",
		Tags:        []string{"Billing"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Limit int `query:"limit" minimum:"1" maximum:"50" default:"20"`
	}) (*ReceiptsOutput, error) {
		if err := selling(svc); err != nil {
			return nil, err
		}

		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		orders, err := svc.Receipts(ctx, p.TenantID, p.UserID, in.Limit)
		if err != nil {
			return nil, commerceError(err)
		}

		out := &ReceiptsOutput{CacheControl: billingCacheControl}
		out.Body.Orders = make([]OrderView, 0, len(orders))
		for _, o := range orders {
			out.Body.Orders = append(out.Body.Orders, orderView(o))
		}
		return out, nil
	})

	/*
		The webhook. Unauthenticated, and that is not an oversight: a gateway has no
		session with us. Its signature is the authentication, and the tenant comes from
		metadata inside the signed payload — the only way a request with no session can
		name a workspace whose tables are behind row-level security.

		It answers 200 to anything it verified, including events it has no use for. A
		400 to a webhook we simply do not care about is a retry storm we asked for.
	*/
	huma.Register(api, huma.Operation{
		OperationID: "gateway-webhook",
		Method:      http.MethodPost,
		Path:        "/v1/webhooks/{gateway}",
		Summary:     "A payment gateway's webhook",
		Description: "Authenticated by signature, never by session. Deduplicated: a gateway delivers the same event more than once, and the second delivery enrols nobody twice.",
		Tags:        []string{"Billing"},
	}, func(ctx context.Context, in *struct {
		Gateway   string `path:"gateway" enum:"stripe,fake"`
		Signature string `header:"X-Signature" doc:"The gateway's signature over the raw body."`
		RawBody   []byte
	}) (*struct{}, error) {
		if err := selling(svc); err != nil {
			return nil, err
		}

		if err := svc.Settle(ctx, in.Gateway, in.RawBody, in.Signature); err != nil {
			return nil, commerceError(err)
		}
		return nil, nil
	})
}

func orderView(o commerce.Order) OrderView {
	return OrderView{
		ID:        o.ID.String(),
		CourseID:  o.CourseID.String(),
		Price:     MoneyView{AmountMinor: o.Price.AmountMinor, Currency: o.Price.Currency},
		Status:    o.Status,
		Gateway:   o.Gateway,
		PaidAt:    o.PaidAt,
		CreatedAt: o.CreatedAt,
	}
}

// commerceError maps the commerce package's sentinels onto status codes. This is the
// only place that translation happens; the domain never imports net/http.
func commerceError(err error) error {
	switch {
	case errors.Is(err, commerce.ErrNotFound):
		return huma.Error404NotFound("Not found.")

	case errors.Is(err, commerce.ErrNoAccount):
		return huma.Error409Conflict("This workspace has no payment account. Connect one before pricing a course.")

	case errors.Is(err, commerce.ErrAccountNotReady):
		return huma.Error409Conflict("The gateway will not take charges on this account yet. Finish onboarding first.")

	case errors.Is(err, commerce.ErrFree):
		return huma.Error409Conflict("This course is free. Enrol on it rather than buying it.")

	case errors.Is(err, commerce.ErrAlreadyOwned):
		return huma.Error409Conflict("You have already bought this course.")

	case errors.Is(err, commerce.ErrInvalidPrice):
		return huma.Error422UnprocessableEntity(err.Error())

	case errors.Is(err, commerce.ErrGatewayUnavailable):
		return huma.Error503ServiceUnavailable("That payment gateway is not configured in this deployment.")

	// 400 and never 401: a WWW-Authenticate on a webhook is an invitation to try
	// again with a password, and there is no password. A signature that does not
	// verify is a request that was not sent by the gateway.
	case errors.Is(err, commerce.ErrSignature):
		return huma.Error400BadRequest("The signature does not verify.")
	}

	return err
}
