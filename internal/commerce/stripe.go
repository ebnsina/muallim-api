package commerce

import (
	"context"

	"github.com/google/uuid"
)

/*
Stripe is the driver for Stripe Connect, and it is not written yet.

It is here as a named, refusing shape rather than as a comment, because the
difference matters: a deployment that configures `stripe` without keys gets
ErrGatewayUnavailable at the door, not a checkout that leads nowhere and an order
that never settles.

What it will do when the keys arrive, and what the shape below already promises:

  - Onboard creates a Connect *Standard* account link. Standard, deliberately: the
    school gets its own Stripe dashboard, and owns its tax, its refunds and its
    chargebacks — because it owns the relationship with the learner. Muallim never
    holds their money, which keeps us out of money transmission.

  - Checkout creates a Checkout Session on the school's connected account with
    `application_fee_amount` as our cut, and puts the order and tenant ids in
    `metadata`. That metadata is what comes back inside the signed webhook, and it
    is the only way an unauthenticated request can name a tenant whose tables are
    behind row-level security.

  - Verify calls `webhook.ConstructEvent` with the signing secret, and maps
    `checkout.session.completed` → EventPaid, `charge.refunded` → EventRefunded,
    `checkout.session.async_payment_failed` → EventFailed, and everything else —
    Stripe sends dozens of kinds — to EventIgnored. A 400 to a webhook we simply do
    not care about is a retry storm we asked for.

The Fake driver beside it exercises every one of those paths today, including the
signature and the idempotency, so what is missing here is the network call and
nothing else.
*/
type Stripe struct {
	// SecretKey and WebhookSecret come from the environment. Empty means unconfigured,
	// and unconfigured means this driver is not registered at all — see cmd/api.
	SecretKey     string
	WebhookSecret string

	// FeeBasisPoints is Muallim's cut, in hundredths of a percent. 250 is 2.5%.
	FeeBasisPoints int64
}

// Name is the gateway this driver serves.
func (Stripe) Name() string { return GatewayStripe }

func (Stripe) Onboard(context.Context, uuid.UUID, string) (Onboarding, error) {
	return Onboarding{}, ErrGatewayUnavailable
}

func (Stripe) AccountStatus(context.Context, string) (Account, error) {
	return Account{}, ErrGatewayUnavailable
}

func (Stripe) Checkout(context.Context, Account, Order, CheckoutURLs) (string, string, error) {
	return "", "", ErrGatewayUnavailable
}

func (Stripe) Verify([]byte, string) (Event, error) {
	return Event{}, ErrGatewayUnavailable
}

// Fee is our cut of an amount, in the same minor units. Written now because it is
// the one piece of arithmetic in the driver, and arithmetic about money is worth
// having a test for before there is a network call wrapped around it.
func (s Stripe) Fee(m Money) int64 {
	return m.AmountMinor * s.FeeBasisPoints / 10_000
}
