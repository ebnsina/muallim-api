package commerce

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	stripego "github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"
)

/*
Stripe is the driver for Stripe Connect *Standard*.

Standard, deliberately: the school gets its own Stripe dashboard and owns its tax,
its refunds and its chargebacks, because it owns the relationship with the learner.
The learner's card is charged on the school's connected account — a direct charge —
and Muallim takes `application_fee_amount` off the top. We never hold their money,
which keeps us out of money transmission.

There are no per-workspace credentials: the platform holds one secret key and acts
on behalf of a connected account with the `Stripe-Account` header, which is what
`SetStripeAccount` sets. `Account.Credentials` is for the gateways that have no such
notion, and this driver ignores it.

The order and tenant ids go in `metadata`, and that metadata is what comes back
inside the signed webhook — the only way an unauthenticated request can name a
tenant whose tables are behind row-level security.
*/
type Stripe struct {
	// SecretKey and WebhookSecret come from the environment. Empty means unconfigured,
	// and every method then refuses: a deployment with no keys fails at the door rather
	// than opening a checkout that leads nowhere.
	SecretKey     string
	WebhookSecret string

	// FeeBasisPoints is Muallim's cut, in hundredths of a percent. 250 is 2.5%.
	FeeBasisPoints int64
}

// NewStripe builds the driver. cmd/ registers it only when the keys are set.
func NewStripe(secretKey, webhookSecret string, feeBasisPoints int64) *Stripe {
	return &Stripe{SecretKey: secretKey, WebhookSecret: webhookSecret, FeeBasisPoints: feeBasisPoints}
}

// The capabilities this driver claims. Stripe signs its webhooks and refunds a
// payment intent; it has nothing to confirm, so it is no Confirmer.
var (
	_ Gateway   = (*Stripe)(nil)
	_ Webhooker = (*Stripe)(nil)
	_ Refunder  = (*Stripe)(nil)
)

// Metadata keys. They travel to Stripe and come back inside the signed payload.
const (
	stripeMetaTenant = "tenant_id"
	stripeMetaOrder  = "order_id"
)

// Name is the gateway this driver serves.
func (Stripe) Name() string { return GatewayStripe }

// client is built per call: the struct is a value, and cmd/ constructs it as a
// literal, so there is no place to keep one that a nil field would not defeat.
func (s Stripe) client() *stripego.Client { return stripego.NewClient(s.SecretKey) }

// configured reports whether this deployment has keys. Without them the driver
// refuses rather than half-working.
func (s Stripe) configured() bool { return s.SecretKey != "" }

/*
Fee is Muallim's cut of an amount, in the same minor units.

Nought or the whole amount is not a fee Stripe will take — it rejects a fee that
equals or exceeds the charge, and a fee of nought is a parameter with nothing to
say — so both are reported as "send no fee" rather than as an invalid one. That is
what happens to a 2.5% cut of a 20p course.
*/
func (s Stripe) Fee(m Money) int64 {
	fee := m.AmountMinor * s.FeeBasisPoints / 10_000
	if fee <= 0 || fee >= m.AmountMinor {
		return 0
	}
	return fee
}

// stripeCurrency is the price's currency as Stripe wants it. Ours is upper-case
// ISO-4217 and Stripe's is lower-case: it refuses "GBP".
func stripeCurrency(m Money) string { return strings.ToLower(m.Currency) }

// Onboard creates a Standard account and a single-use link to Stripe's onboarding.
// The link expires, so it is minted each time rather than stored.
func (s Stripe) Onboard(ctx context.Context, tenantID uuid.UUID, returnURL string) (Onboarding, error) {
	if !s.configured() {
		return Onboarding{}, ErrGatewayUnavailable
	}
	sc := s.client()

	create := &stripego.AccountCreateParams{
		Type:     stripego.String(string(stripego.AccountTypeStandard)),
		Metadata: map[string]string{stripeMetaTenant: tenantID.String()},
	}
	create.SetIdempotencyKey("onboard-" + tenantID.String())

	account, err := sc.V1Accounts.Create(ctx, create)
	if err != nil {
		return Onboarding{}, fmt.Errorf("stripe: create connected account: %w", err)
	}

	link := &stripego.AccountLinkCreateParams{
		Account: stripego.String(account.ID),
		Type:    stripego.String(string(stripego.AccountLinkTypeAccountOnboarding)),

		// Refresh and return are the same page: it reads the account's status back and
		// either offers the link again or says the school is connected.
		RefreshURL: stripego.String(returnURL),
		ReturnURL:  stripego.String(returnURL),
	}

	created, err := sc.V1AccountLinks.Create(ctx, link)
	if err != nil {
		return Onboarding{}, fmt.Errorf("stripe: create account link: %w", err)
	}
	return Onboarding{URL: created.URL, ExternalID: account.ID}, nil
}

// AccountStatus asks Stripe what it will actually do with this account, rather than
// inferring it from the fact that somebody finished a form.
func (s Stripe) AccountStatus(ctx context.Context, account Account) (Account, error) {
	if !s.configured() {
		return Account{}, ErrGatewayUnavailable
	}
	if account.ExternalID == "" {
		return Account{}, fmt.Errorf("%w: the account has no stripe id", ErrNotFound)
	}

	got, err := s.client().V1Accounts.GetByID(ctx, account.ExternalID, &stripego.AccountRetrieveParams{})
	if err != nil {
		return Account{}, fmt.Errorf("stripe: retrieve account %s: %w", account.ExternalID, err)
	}

	account.ChargesEnabled = got.ChargesEnabled
	account.Status = stripeAccountStatus(got)
	return account, nil
}

// stripeAccountStatus reads Stripe's three booleans as one status. Restricted is
// distinguished from pending only when Stripe has actually said no — a disabled
// reason or a requirement already past its deadline — because "we are still waiting
// for you" and "we have stopped you" are different things to tell a school.
func stripeAccountStatus(a *stripego.Account) string {
	if a.ChargesEnabled && a.DetailsSubmitted {
		return AccountActive
	}
	if r := a.Requirements; r != nil && (r.DisabledReason != "" || len(r.PastDue) > 0) {
		return AccountRestricted
	}
	return AccountPending
}

/*
Checkout opens a hosted Checkout Session on the school's own account.

The card never touches this server: the page is Stripe's, which is a compliance
problem we have no reason to own. The fee rides on the payment intent, and the ids
ride in the metadata of *both* the session and the payment intent — session metadata
does not propagate to the payment intent, and the refund webhook only ever sees the
latter.
*/
func (s Stripe) Checkout(ctx context.Context, account Account, order Order, urls CheckoutURLs) (string, string, error) {
	if !s.configured() {
		return "", "", ErrGatewayUnavailable
	}
	if account.ExternalID == "" {
		return "", "", fmt.Errorf("%w: the account has no stripe id", ErrNotFound)
	}

	meta := map[string]string{
		stripeMetaTenant: order.TenantID.String(),
		stripeMetaOrder:  order.ID.String(),
	}

	intent := &stripego.CheckoutSessionCreatePaymentIntentDataParams{Metadata: meta}
	if fee := s.Fee(order.Price); fee > 0 {
		intent.ApplicationFeeAmount = stripego.Int64(fee)
	}

	create := &stripego.CheckoutSessionCreateParams{
		Mode:       stripego.String(string(stripego.CheckoutSessionModePayment)),
		SuccessURL: stripego.String(urls.Success),
		CancelURL:  stripego.String(urls.Cancel),
		LineItems: []*stripego.CheckoutSessionCreateLineItemParams{{
			Quantity: stripego.Int64(1),
			PriceData: &stripego.CheckoutSessionCreateLineItemPriceDataParams{
				Currency:   stripego.String(stripeCurrency(order.Price)),
				UnitAmount: stripego.Int64(order.Price.AmountMinor),
				ProductData: &stripego.CheckoutSessionCreateLineItemPriceDataProductDataParams{
					Name: stripego.String("Course " + order.CourseID.String()),
				},
			},
		}},
		PaymentIntentData: intent,
		Metadata:          meta,
	}

	// The header that makes this the school's charge and not ours. Without it the
	// learner pays Muallim, which is the one thing this architecture exists to avoid.
	create.SetStripeAccount(account.ExternalID)

	// Keyed on the order, so a learner who double-clicks gets one session, not two.
	create.SetIdempotencyKey("checkout-" + order.ID.String())

	session, err := s.client().V1CheckoutSessions.Create(ctx, create)
	if err != nil {
		return "", "", fmt.Errorf("stripe: create checkout session for order %s: %w", order.ID, err)
	}
	return session.URL, session.ID, nil
}

/*
Verify authenticates a webhook and says what it means.

The signature is the whole authentication — a gateway has no session with us — so a
payload that does not verify is somebody attacking, and is refused before it is
parsed. Everything Stripe sends that we have no use for, and it sends dozens of
kinds, is EventIgnored rather than an error: a 400 to a webhook we do not care about
is a retry storm we asked for.
*/
func (s Stripe) Verify(payload []byte, signature string) (Event, error) {
	if !s.configured() || s.WebhookSecret == "" {
		return Event{}, ErrGatewayUnavailable
	}

	event, err := webhook.ConstructEvent(payload, signature, s.WebhookSecret)
	if err != nil {
		return Event{}, fmt.Errorf("%w: %v", ErrSignature, err)
	}

	switch event.Type {
	case stripego.EventTypeCheckoutSessionCompleted, stripego.EventTypeCheckoutSessionAsyncPaymentSucceeded:
		session, err := stripeSession(event.Data.Raw)
		if err != nil {
			return Event{}, err
		}

		// Completed is not paid. A delayed method — a bank debit — completes the session
		// and settles days later, and enrolling on that would be giving the course away.
		if session.PaymentStatus != stripego.CheckoutSessionPaymentStatusPaid {
			return Event{Kind: EventIgnored}, nil
		}
		return stripeEvent(EventPaid, session)

	case stripego.EventTypeCheckoutSessionAsyncPaymentFailed:
		session, err := stripeSession(event.Data.Raw)
		if err != nil {
			return Event{}, err
		}
		return stripeEvent(EventFailed, session)

	case stripego.EventTypeChargeRefunded:
		// Ignored, and not for want of trying: a charge names no checkout session, and
		// Service.apply refuses any event whose ExternalID is not the order's session id.
		// Our own refunds settle in the transaction that issues them, so nothing is lost
		// but a refund made by hand in the school's Stripe dashboard — and applying that
		// needs the service to match a refund on PaymentExternalID instead.
		return Event{Kind: EventIgnored}, nil

	default:
		return Event{Kind: EventIgnored}, nil
	}
}

// stripeSession is the session inside a webhook, which arrives unexpanded.
func stripeSession(raw json.RawMessage) (*stripego.CheckoutSession, error) {
	var session stripego.CheckoutSession
	if err := json.Unmarshal(raw, &session); err != nil {
		return nil, fmt.Errorf("stripe: decode checkout session: %w", err)
	}
	return &session, nil
}

// stripeEvent reads the ids back out of the metadata we set when the session was
// created. Absent or malformed is not a webhook we can act on: an event that names
// no order is an event with nothing to settle.
func stripeEvent(kind EventKind, session *stripego.CheckoutSession) (Event, error) {
	tenantID, err := uuid.Parse(session.Metadata[stripeMetaTenant])
	if err != nil {
		return Event{}, fmt.Errorf("%w: the event names no tenant", ErrNotFound)
	}
	orderID, err := uuid.Parse(session.Metadata[stripeMetaOrder])
	if err != nil {
		return Event{}, fmt.Errorf("%w: the event names no order", ErrNotFound)
	}

	// The payment intent, which is what a refund is later issued against. It is a bare
	// id here, not an expanded object, and on a failed payment there may be none at all.
	var payment string
	if session.PaymentIntent != nil {
		payment = session.PaymentIntent.ID
	}

	return Event{
		Kind:              kind,
		TenantID:          tenantID,
		OrderID:           orderID,
		ExternalID:        session.ID,
		PaymentExternalID: payment,
	}, nil
}

/*
Refund gives the learner's money back, out of the school's balance.

RefundApplicationFee, because without it Stripe keeps our cut and takes the whole
refund out of the school: we would be charging a school a fee for a sale that did
not happen. A nil Amount is a full refund, which is the only kind this API offers.
*/
func (s Stripe) Refund(ctx context.Context, account Account, order Order) (string, error) {
	if !s.configured() {
		return "", ErrGatewayUnavailable
	}
	if order.PaymentExternalID == "" {
		return "", fmt.Errorf("%w: the order has no payment to refund", ErrNotPaid)
	}

	refund := &stripego.RefundCreateParams{
		PaymentIntent:        stripego.String(order.PaymentExternalID),
		RefundApplicationFee: stripego.Bool(true),
	}
	refund.SetStripeAccount(account.ExternalID)

	// Keyed on the order, because a retried refund must not pay the learner twice.
	refund.SetIdempotencyKey("refund-" + order.ID.String())

	created, err := s.client().V1Refunds.Create(ctx, refund)
	if err != nil {
		return "", fmt.Errorf("stripe: refund payment %s: %w", order.PaymentExternalID, err)
	}
	return created.ID, nil
}
