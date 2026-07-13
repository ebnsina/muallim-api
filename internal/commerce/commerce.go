// Package commerce sells a course.
//
// The workspace is the merchant, not Muallim: the learner pays the school's own
// connected gateway account, and Muallim takes a fee off the top. That is the
// whole architecture. Collecting a learner's money for somebody else's course
// would make us liable for their tax, their refunds and their chargebacks, and in
// most jurisdictions is money transmission — a licence, not a feature.
//
// It knows nothing about HTTP, and imports no sibling. A paid order becomes an
// enrolment through an interface this package declares and cmd/ wires over enroll.
package commerce

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors. internal/httpapi maps them; nothing else does.
var (
	ErrNotFound = errors.New("commerce: not found")

	// ErrNoAccount means the workspace has not connected a gateway account, so it
	// cannot be paid. A course may not be priced until it can be.
	ErrNoAccount = errors.New("commerce: the workspace has no payment account")

	// ErrAccountNotReady means the account exists and the gateway will not yet take
	// charges on it. Onboarding is not finished, or the gateway has restricted it.
	ErrAccountNotReady = errors.New("commerce: the payment account cannot take charges")

	// ErrFree means somebody tried to buy a course that costs nothing. It is not an
	// error the learner caused: they should have been enrolled, not sent to pay.
	ErrFree = errors.New("commerce: the course is free")

	// ErrAlreadyOwned means this learner has already paid for this course. Charging
	// them twice for the same thing is the one mistake a shop cannot apologise for.
	ErrAlreadyOwned = errors.New("commerce: the learner already owns this course")

	// ErrInvalidPrice is a price the API refuses: nought, negative, or a currency
	// that is not three letters.
	ErrInvalidPrice = errors.New("commerce: the price is not valid")

	// ErrGatewayUnavailable means the driver for that gateway is not configured in
	// this deployment. Saying so beats a checkout that leads nowhere.
	ErrGatewayUnavailable = errors.New("commerce: no driver serves that gateway")

	// ErrSignature means a webhook did not come from the gateway it claims to. It is
	// the one error in this package that is somebody attacking, not somebody wrong.
	ErrSignature = errors.New("commerce: the webhook signature does not verify")

	// ErrNotPaid means somebody tried to refund an order that never took any money.
	ErrNotPaid = errors.New("commerce: the order is not paid")

	// ErrUnsupported means the driver cannot do what was asked of it — refund a
	// payment, verify a webhook it never sends. Said plainly, because the alternative
	// is a button that silently does nothing.
	ErrUnsupported = errors.New("commerce: the gateway does not support that")

	// ErrCredentials means the workspace's own gateway credentials are missing or
	// will not decrypt. SSLCommerz and bKash need them; Stripe does not.
	ErrCredentials = errors.New("commerce: the gateway credentials are missing")
)

// Gateways. `fake` is not a stub for tests to reach for — it is a real driver a
// deployment can run, and it is how a deployment with no keys still exercises the
// whole flow.
const (
	GatewayStripe     = "stripe"
	GatewaySSLCommerz = "sslcommerz"
	GatewayBkash      = "bkash"
	GatewayFake       = "fake"
)

// Account statuses.
const (
	AccountPending    = "pending"
	AccountActive     = "active"
	AccountRestricted = "restricted"
)

// Order statuses.
const (
	OrderPending  = "pending"
	OrderPaid     = "paid"
	OrderFailed   = "failed"
	OrderRefunded = "refunded"
)

// Money is an amount in a currency's smallest unit, and the currency.
//
// Minor units and never a float: 0.1 + 0.2 is not 0.3 in binary, and the penny it
// loses is a penny somebody's accountant will find. `1200 BDT` is twelve taka.
type Money struct {
	AmountMinor int64
	Currency    string
}

// Account is a workspace's account with a gateway.
type Account struct {
	ID       uuid.UUID
	TenantID uuid.UUID

	Gateway string

	// ExternalID is the gateway's own id for the account. Never composed, never
	// guessed: it is what the gateway told us, and what we send back to it.
	ExternalID string

	Status string

	// ChargesEnabled is what the gateway says it will actually do. An account can be
	// onboarded and still be refused charges, so it is asked rather than inferred.
	ChargesEnabled bool

	// Credentials is the workspace's own merchant secret, decrypted, and only for the
	// gateways that have one. It is never persisted from here and never returned by
	// the API: the service fills it in on its way to a driver, and nowhere else.
	Credentials Credentials

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Ready reports whether a course priced against this account could actually be sold.
func (a Account) Ready() bool {
	return a.Status == AccountActive && a.ChargesEnabled
}

// Order is one learner's attempt to buy one course.
type Order struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	CourseID uuid.UUID
	UserID   uuid.UUID

	// Price as it stood at the moment of sale. A school that later raises it must not
	// rewrite what somebody already paid.
	Price Money

	Status  string
	Gateway string

	// ExternalID is the gateway's id for the checkout, and the key a webhook is
	// matched on. Unique with the gateway, which is what makes a replayed webhook a
	// no-op rather than a second enrolment.
	ExternalID string

	// PaymentExternalID is the gateway's id for the *payment*, learned when the money
	// moved. A refund is issued against this, not against the session that led to it:
	// bKash refunds a trxID, Stripe refunds a payment intent, and neither will take a
	// checkout id in its place.
	PaymentExternalID string

	// RefundExternalID is what the gateway called the refund, kept so a support
	// question about somebody's money has an answer.
	RefundExternalID string

	PaidAt     *time.Time
	RefundedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Checkout is where to send the learner, and the order it belongs to.
type Checkout struct {
	Order Order

	// URL is the gateway's hosted page. It is the gateway's, never ours: a card
	// number that touches this server is a compliance problem we have no reason to own.
	URL string
}

// EventKind is what a gateway is telling us happened.
type EventKind string

const (
	EventPaid     EventKind = "paid"
	EventFailed   EventKind = "failed"
	EventRefunded EventKind = "refunded"

	// EventIgnored is a webhook we understood and have no use for. It is not an
	// error: a gateway sends dozens of kinds, and a 400 to any of them is a retry
	// storm we asked for.
	EventIgnored EventKind = "ignored"
)

/*
Event is a verified webhook.

TenantID comes from the metadata we set when the checkout was created and the
gateway echoed back inside the signed payload — so it is ours, and it is signed.
It has to come from there: an unauthenticated webhook has no session, and the
tables it must touch are behind row-level security that answers a query with no
tenant by returning nothing at all.
*/
type Event struct {
	Kind EventKind

	TenantID uuid.UUID
	OrderID  uuid.UUID

	// ExternalID is the gateway's id for the checkout, checked against the order's
	// own: metadata we can read is metadata that could have been edited in the
	// gateway's dashboard.
	ExternalID string

	// PaymentExternalID is the gateway's id for the payment itself, if the event
	// carried one. It is what a refund will later be issued against.
	PaymentExternalID string
}

/*
Credentials are a workspace's own merchant credentials with a gateway.

Stripe does not need them — the platform holds one key and acts on behalf of the
connected account — but SSLCommerz and bKash have no such notion: each school *is*
the merchant, with its own store id and its own secret. Those secrets are held
encrypted, and they are handed to a driver at the moment it needs them and never
returned by the API, logged, or put in an audit entry.
*/
type Credentials struct {
	// PublicID is the store id, the app key: needed to build a request, and not secret.
	PublicID string

	// Secret is the store password, the app secret. Decrypted only in memory.
	Secret string
}

// Has reports whether both halves are present.
func (c Credentials) Has() bool { return c.PublicID != "" && c.Secret != "" }

/*
Gateway is a payment provider, from the outside — and only the part every provider
actually has.

`Checkout` opens a hosted session on the *school's* account and says where to send
the learner. That, onboarding, and asking the account what it will do are the whole
of the common ground.

How the money is *confirmed* is not common ground, and pretending otherwise is how
a driver ends up lying. Stripe tells us in a signed webhook. bKash sends no webhook
at all: the learner comes back to a callback URL, and the only proof is a
server-to-server execute. SSLCommerz does both. So confirmation, and refunding, are
capabilities a driver declares — `Webhooker`, `Confirmer`, `Refunder` — and the
service asks whether a driver has them rather than making every driver pretend.
*/
type Gateway interface {
	// Name is the constant this driver serves: `stripe`, `sslcommerz`, `bkash`, `fake`.
	Name() string

	// Onboard returns where to send an author to connect their account, and the
	// gateway's id for it. Called once per workspace.
	Onboard(ctx context.Context, tenantID uuid.UUID, returnURL string) (Onboarding, error)

	// AccountStatus asks the gateway what it will actually do with this account.
	AccountStatus(ctx context.Context, account Account) (Account, error)

	// Checkout opens a hosted session on the connected account. The order is already
	// written and pending: a session that succeeds against an order that does not
	// exist is money we cannot account for. It returns where to send the learner and
	// the gateway's id for the session.
	Checkout(ctx context.Context, account Account, order Order, urls CheckoutURLs) (string, string, error)
}

// Webhooker is a gateway that tells us, out of band and signed, what happened.
type Webhooker interface {
	// Verify authenticates a raw webhook and says what it means. The signature is
	// whatever header the gateway signs with; a driver that signs the body itself
	// ignores it.
	Verify(payload []byte, signature string) (Event, error)
}

/*
Confirmer is a gateway whose truth is a question we ask, not an answer it sends.

The learner returns to our callback with a payment id in the query string. That is
not proof of anything — anybody can type a URL — so the driver goes back to the
gateway with it and asks what really happened. bKash works this way and only this
way; SSLCommerz's validation API is the same shape and is the authority even when
its IPN also arrived.
*/
type Confirmer interface {
	Confirm(ctx context.Context, account Account, order Order, query map[string]string) (Event, error)
}

// Refunder is a gateway that can give the money back. It refunds the *payment* —
// order.PaymentExternalID — and returns the gateway's id for the refund.
type Refunder interface {
	Refund(ctx context.Context, account Account, order Order) (string, error)
}

// RefundState is what a gateway says has become of a refund it accepted.
type RefundState string

const (
	// RefundDone: the money has moved. There is nothing left to chase.
	RefundDone RefundState = "done"

	// RefundPending: the gateway accepted it and has not finished. Ask again later.
	RefundPending RefundState = "pending"

	// RefundFailed: the gateway will not, after all, give the money back. The order is
	// already marked refunded and the enrolment already withdrawn, so this is the one
	// state that needs a person: the learner lost the course and never got the money.
	RefundFailed RefundState = "failed"
)

/*
RefundConfirmer is a gateway whose refund is not final when it is accepted.

SSLCommerz refunds asynchronously: the initiate call returns `processing`, and
whether the money actually moved is a separate question, asked against the refund's
own id. A driver that settles a refund synchronously — Stripe, bKash, the fake —
does not implement this, and no confirmation is scheduled for it.
*/
type RefundConfirmer interface {
	RefundStatus(ctx context.Context, account Account, order Order) (RefundState, error)
}

// Onboarding is where to send an author, and what the gateway will call them.
type Onboarding struct {
	URL        string
	ExternalID string
}

// CheckoutURLs are where the gateway returns the learner, whichever way it went.
type CheckoutURLs struct {
	Success string
	Cancel  string
}
