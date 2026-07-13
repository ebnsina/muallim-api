package commerce

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
)

/*
The URL a driver hands the gateway must be a URL this API answers.

It was not, once. The callback was registered as a JSON POST while bKash sends the
browser back with a GET, and neither URL named the workspace — which every table
behind them is filtered by. Nothing failed to compile and no test went red: the
gateways would simply have called a 404 for ever, and not one payment would have
settled. So this reads what the driver actually *sends*, rather than what this file
thinks it sends, and checks it against the paths `internal/httpapi` registers.
*/
func TestTheCallbackURLsNameTheWorkspaceAndTheOrder(t *testing.T) {
	t.Parallel()

	order := Order{
		ID:       uuid.New(),
		TenantID: uuid.New(),
		Price:    Money{AmountMinor: 150000, Currency: "BDT"},
	}
	suffix := "/" + order.TenantID.String() + "/" + order.ID.String()

	t.Run("bkash", func(t *testing.T) {
		t.Parallel()

		var sent string
		gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/token/grant") {
				_ = json.NewEncoder(w).Encode(map[string]any{"id_token": "t", "expires_in": 3600})
				return
			}

			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			sent = body["callbackURL"]

			_ = json.NewEncoder(w).Encode(map[string]string{
				"statusCode": "0000", "paymentID": "TR001", "bkashURL": "https://pay.bkash/x",
			})
		}))
		defer gateway.Close()

		driver := NewBkash(true, "https://api.example.com/v1/payments/bkash/callback", gateway.Client())
		driver.host = gateway.URL

		account := Account{
			Credentials: Credentials{
				PublicID: "app-key",
				Secret:   `{"app_secret":"s","username":"u","password":"p"}`,
			},
		}
		if _, _, err := driver.Checkout(t.Context(), account, order, CheckoutURLs{}); err != nil {
			t.Fatalf("Checkout: %v", err)
		}

		// GET /v1/payments/bkash/callback/{tenant}/{order}
		want := "https://api.example.com/v1/payments/bkash/callback" + suffix
		if sent != want {
			t.Errorf("bKash is told to call\n  %s\nbut this API answers\n  %s", sent, want)
		}
	})

	t.Run("sslcommerz", func(t *testing.T) {
		t.Parallel()

		var sent string
		gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(body))
			sent = form.Get("ipn_url")

			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "SUCCESS", "sessionkey": "sk", "GatewayPageURL": "https://pay.ssl/x",
			})
		}))
		defer gateway.Close()

		driver, err := NewSSLCommerz(gateway.URL,
			"https://api.example.com/v1/payments/sslcommerz/ipn", gateway.Client())
		if err != nil {
			t.Fatalf("NewSSLCommerz: %v", err)
		}

		account := Account{
			Credentials: Credentials{PublicID: "store", Secret: "password"},
		}
		if _, _, err := driver.Checkout(t.Context(), account, order, CheckoutURLs{
			Success: "https://app.example.com/ok", Cancel: "https://app.example.com/no",
		}); err != nil {
			t.Fatalf("Checkout: %v", err)
		}

		// POST /v1/payments/sslcommerz/ipn/{tenant}/{order}
		want := "https://api.example.com/v1/payments/sslcommerz/ipn" + suffix
		if sent != want {
			t.Errorf("SSLCommerz is told to post to\n  %s\nbut this API answers\n  %s", sent, want)
		}
	})
}
