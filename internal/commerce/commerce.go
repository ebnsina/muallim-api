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
)

// Gateways. `fake` is not a stub for tests to reach for — it is a real driver a
// deployment can run, and it is how this ships until the Stripe keys arrive.
const (
	GatewayStripe = "stripe"
	GatewayFake   = "fake"
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
}

/*
Gateway is a payment provider, from the outside.

Declared here by the package that needs it. `Checkout` creates a hosted session on
the *school's* connected account and returns where to send the learner. `Verify`
turns a raw webhook body into an Event, or refuses it.

Both are the whole surface. A driver that needed a third method would be a driver
telling us its model does not fit — which is worth hearing before it is wired in.
*/
type Gateway interface {
	// Name is the constant this driver serves: `stripe`, `fake`.
	Name() string

	// Onboard returns where to send an author to connect their account, and the
	// gateway's id for it. Called once per workspace.
	Onboard(ctx context.Context, tenantID uuid.UUID, returnURL string) (Onboarding, error)

	// AccountStatus asks the gateway what it will actually do with this account.
	AccountStatus(ctx context.Context, externalID string) (Account, error)

	// Checkout opens a hosted session on the connected account. The order is already
	// written and pending: a session that succeeds against an order that does not
	// exist is money we cannot account for.
	Checkout(ctx context.Context, account Account, order Order, urls CheckoutURLs) (string, string, error)

	// Verify authenticates a raw webhook and says what it means.
	Verify(payload []byte, signature string) (Event, error)
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
