package commerce

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// The fee is the only arithmetic in the driver, and arithmetic about money is the
// thing worth pinning down before a network call is wrapped around it.
func TestStripeFee(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		bps   int64
		money Money
		want  int64
	}{
		{"two and a half percent of a tenner", 250, Money{AmountMinor: 1000, Currency: "GBP"}, 25},
		{"rounds down, never up", 250, Money{AmountMinor: 999, Currency: "GBP"}, 24},
		{"a fee of nought is no fee", 250, Money{AmountMinor: 20, Currency: "GBP"}, 0},
		{"a fee is never the whole charge", 10_000, Money{AmountMinor: 1000, Currency: "GBP"}, 0},
		{"nor more than it", 12_000, Money{AmountMinor: 1000, Currency: "GBP"}, 0},
		{"no cut configured, no cut taken", 0, Money{AmountMinor: 100_000, Currency: "GBP"}, 0},
		{"minor units, not pounds", 250, Money{AmountMinor: 1_000_000, Currency: "BDT"}, 25_000},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			got := NewStripe("sk_test", "whsec_test", c.bps).Fee(c.money)
			if got != c.want {
				t.Fatalf("Fee(%d minor at %d bps) = %d, want %d", c.money.AmountMinor, c.bps, got, c.want)
			}
		})
	}
}

// Ours is upper-case ISO-4217 and Stripe's is lower-case: it refuses "GBP".
func TestStripeCurrency(t *testing.T) {
	t.Parallel()

	cases := map[string]string{"GBP": "gbp", "USD": "usd", "BDT": "bdt", "eur": "eur"}

	for in, want := range cases {
		if got := stripeCurrency(Money{AmountMinor: 100, Currency: in}); got != want {
			t.Fatalf("stripeCurrency(%q) = %q, want %q", in, got, want)
		}
	}
}

// Unconfigured must refuse at the door. A deployment with no keys that opened a
// checkout leading nowhere would be worse than one that plainly says it cannot.
func TestStripeUnconfiguredRefuses(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	unconfigured := NewStripe("", "", 250)

	cases := []struct {
		name string
		call func() error
	}{
		{"Onboard", func() error {
			_, err := unconfigured.Onboard(ctx, uuid.New(), "https://example.test/return")
			return err
		}},
		{"AccountStatus", func() error {
			_, err := unconfigured.AccountStatus(ctx, Account{ExternalID: "acct_123"})
			return err
		}},
		{"Checkout", func() error {
			_, _, err := unconfigured.Checkout(ctx, Account{ExternalID: "acct_123"}, Order{ID: uuid.New()}, CheckoutURLs{})
			return err
		}},
		{"Verify", func() error {
			_, err := unconfigured.Verify([]byte(`{}`), "t=1,v1=deadbeef")
			return err
		}},
		{"Refund", func() error {
			_, err := unconfigured.Refund(ctx, Account{ExternalID: "acct_123"}, Order{PaymentExternalID: "pi_123"})
			return err
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if err := c.call(); !errors.Is(err, ErrGatewayUnavailable) {
				t.Fatalf("%s with no keys = %v, want ErrGatewayUnavailable", c.name, err)
			}
		})
	}
}

// A driver with a secret key but no webhook secret cannot authenticate a webhook,
// and must say so rather than trust one.
func TestStripeVerifyWithoutWebhookSecret(t *testing.T) {
	t.Parallel()

	_, err := NewStripe("sk_test", "", 250).Verify([]byte(`{}`), "t=1,v1=deadbeef")
	if !errors.Is(err, ErrGatewayUnavailable) {
		t.Fatalf("Verify with no webhook secret = %v, want ErrGatewayUnavailable", err)
	}
}

// A payload that does not verify is somebody attacking, not somebody wrong.
func TestStripeVerifyBadSignature(t *testing.T) {
	t.Parallel()

	_, err := NewStripe("sk_test", "whsec_test", 250).Verify([]byte(`{"type":"charge.refunded"}`), "t=1,v1=notasignature")
	if !errors.Is(err, ErrSignature) {
		t.Fatalf("Verify with a forged signature = %v, want ErrSignature", err)
	}
}
