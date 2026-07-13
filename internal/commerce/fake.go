package commerce

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

/*
Fake is a gateway that takes no money.

It is a real driver, not a test double: a deployment with no Stripe keys runs it,
the whole flow works end to end, and the only thing that does not happen is the
charge. That is what makes it useful — the checkout, the webhook, the signature,
the idempotency and the enrolment are exercised by every developer on every run,
rather than by a mock nobody trusts.

Its webhooks are signed, with the same HMAC shape Stripe uses. A fake that skipped
the signature would leave the one line of the real driver that matters untested.
*/
type Fake struct {
	// BaseURL is where the fake's own checkout page lives — in this deployment, a
	// page the web app serves that offers "pay" and "cancel" buttons.
	BaseURL string

	// Secret signs its webhooks. Not a security boundary — nothing here is worth
	// forging — but the code path is the real one.
	Secret string
}

// Name is the gateway this driver serves.
func (Fake) Name() string { return GatewayFake }

// Onboard hands back an account that is ready immediately. There is nobody to
// verify, and no bank to wait for.
func (f Fake) Onboard(_ context.Context, tenantID uuid.UUID, returnURL string) (Onboarding, error) {
	return Onboarding{
		URL:        fmt.Sprintf("%s?return_url=%s", returnURL, "connected"),
		ExternalID: "acct_fake_" + tenantID.String()[:8],
	}, nil
}

// AccountStatus says yes. A fake account that could be restricted would be a fake
// with an opinion, and it has none.
func (f Fake) AccountStatus(_ context.Context, account Account) (Account, error) {
	account.Status, account.ChargesEnabled = AccountActive, true
	return account, nil
}

// Checkout returns a URL the web app can render as a page with two buttons, and the
// session id the webhook will name.
func (f Fake) Checkout(_ context.Context, _ Account, order Order, urls CheckoutURLs) (string, string, error) {
	session := "cs_fake_" + order.ID.String()

	url := fmt.Sprintf(
		"%s/checkout/%s?order=%s&tenant=%s&amount=%d&currency=%s&success=%s&cancel=%s",
		f.BaseURL, session, order.ID, order.TenantID, order.Price.AmountMinor, order.Price.Currency,
		urls.Success, urls.Cancel,
	)
	return url, session, nil
}

// FakeEvent is the body the fake's checkout page posts back. It is the shape a real
// gateway's payload is reduced to, and nothing else.
type FakeEvent struct {
	Kind     string `json:"kind"`
	TenantID string `json:"tenant_id"`
	OrderID  string `json:"order_id"`
	Session  string `json:"session"`
}

// Sign is what the fake's checkout page calls to sign what it is about to send.
// Exported because the page is a client, and a client that cannot sign is a client
// whose webhooks are all refused.
func (f Fake) Sign(payload []byte) string {
	mac := hmac.New(sha256.New, []byte(f.Secret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify authenticates a webhook and says what it means.
func (f Fake) Verify(payload []byte, signature string) (Event, error) {
	// Constant time, because a comparison that returns early on the first wrong byte
	// tells an attacker how many bytes they got right.
	want, err := hex.DecodeString(signature)
	if err != nil || !hmac.Equal([]byte(f.Sign(payload)), []byte(hex.EncodeToString(want))) {
		return Event{}, ErrSignature
	}

	var body FakeEvent
	if err := json.Unmarshal(payload, &body); err != nil {
		return Event{}, fmt.Errorf("%w: %v", ErrSignature, err)
	}

	tenantID, err := uuid.Parse(body.TenantID)
	if err != nil {
		return Event{}, fmt.Errorf("%w: tenant", ErrNotFound)
	}
	orderID, err := uuid.Parse(body.OrderID)
	if err != nil {
		return Event{}, fmt.Errorf("%w: order", ErrNotFound)
	}

	kind := EventIgnored
	switch body.Kind {
	case "paid":
		kind = EventPaid
	case "refunded":
		kind = EventRefunded
	case "failed":
		kind = EventFailed
	}

	// A real gateway learns the payment's own id only when the money moves, and a
	// refund is issued against that rather than against the session. The fake says so
	// too, or the refund path would never be exercised until the day it had to be.
	return Event{
		Kind: kind, TenantID: tenantID, OrderID: orderID,
		ExternalID:        body.Session,
		PaymentExternalID: "pi_fake_" + orderID.String(),
	}, nil
}

// Refund takes no money, so it gives none back — but it answers like a gateway that
// did, which is what the flow above it is entitled to expect from a driver.
func (f Fake) Refund(_ context.Context, _ Account, order Order) (string, error) {
	if order.Status != OrderPaid {
		return "", ErrNotPaid
	}
	return "re_fake_" + order.ID.String(), nil
}

// The fake is a real driver, and these are the capabilities it really has.
var (
	_ Gateway   = (*Fake)(nil)
	_ Webhooker = (*Fake)(nil)
	_ Refunder  = (*Fake)(nil)
)
