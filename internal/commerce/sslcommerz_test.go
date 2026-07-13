package commerce

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func sslAccount() Account {
	return Account{
		ID:             uuid.New(),
		TenantID:       uuid.New(),
		Gateway:        GatewaySSLCommerz,
		Status:         AccountActive,
		ChargesEnabled: true,
		Credentials:    Credentials{PublicID: "muallim0live", Secret: "muallim0live@ssl"},
	}
}

func sslOrder(account Account, minor int64, currency string) Order {
	return Order{
		ID:       uuid.New(),
		TenantID: account.TenantID,
		CourseID: uuid.New(),
		UserID:   uuid.New(),
		Price:    Money{AmountMinor: minor, Currency: currency},
		Status:   OrderPending,
		Gateway:  GatewaySSLCommerz,
	}
}

// signIPN runs the documented hash by hand: the verify_key fields plus store_passwd
// as md5(password), sorted by name, joined k=v&k=v, md5, lowercase hex.
func signIPN(t *testing.T, fields url.Values, storePassword string) url.Values {
	t.Helper()

	names := strings.Split(fields.Get("verify_key"), ",")
	pass := md5.Sum([]byte(storePassword))

	pairs := map[string]string{"store_passwd": hex.EncodeToString(pass[:])}
	for _, name := range names {
		if fields.Has(name) {
			pairs[name] = fields.Get(name)
		}
	}

	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	joined := make([]string, 0, len(keys))
	for _, k := range keys {
		joined = append(joined, k+"="+pairs[k])
	}

	sum := md5.Sum([]byte(strings.Join(joined, "&")))
	fields.Set("verify_sign", hex.EncodeToString(sum[:]))
	return fields
}

func sslIPN(order Order, valID, bankTranID string) url.Values {
	return url.Values{
		"status":       {"VALID"},
		"tran_id":      {order.ID.String()},
		"val_id":       {valID},
		"amount":       {minorToMajor(order.Price.AmountMinor)},
		"currency":     {order.Price.Currency},
		"bank_tran_id": {bankTranID},
		"value_a":      {order.TenantID.String()},
		"value_b":      {order.ID.String()},
		"verify_key":   {"amount,bank_tran_id,currency,status,tran_id,val_id,value_a,value_b"},
	}
}

func toMap(v url.Values) map[string]string {
	m := make(map[string]string, len(v))
	for k := range v {
		m[k] = v.Get(k)
	}
	return m
}

func sslDriver(t *testing.T, handler http.HandlerFunc) *SSLCommerz {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	driver, err := NewSSLCommerz(server.URL, "https://api.muallim.test/v1/payments/sslcommerz/ipn", server.Client())
	if err != nil {
		t.Fatalf("NewSSLCommerz: %v", err)
	}
	return driver
}

func TestSSLCommerzCheckout(t *testing.T) {
	t.Parallel()

	account := sslAccount()
	order := sslOrder(account, 120_000, "BDT") // 1200.00 BDT

	var got url.Values
	driver := sslDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gwprocess/v4/api.php" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		got = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"SUCCESS","sessionkey":"sess_1","GatewayPageURL":"https://sandbox.sslcommerz.com/EasyCheckOut/sess_1"}`))
	})

	page, externalID, err := driver.Checkout(context.Background(), account, order,
		CheckoutURLs{Success: "https://muallim.test/paid", Cancel: "https://muallim.test/cancelled"})
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	if page != "https://sandbox.sslcommerz.com/EasyCheckOut/sess_1" {
		t.Errorf("page = %q", page)
	}
	// The IPN echoes tran_id, never the session key, so tran_id is the dedupe key.
	if externalID != order.ID.String() {
		t.Errorf("externalID = %q, want the order id %q", externalID, order.ID)
	}

	want := map[string]string{
		"store_id":     account.Credentials.PublicID,
		"store_passwd": account.Credentials.Secret,
		"total_amount": "1200.00",
		"currency":     "BDT",
		"tran_id":      order.ID.String(),
		"success_url":  "https://muallim.test/paid",
		"cancel_url":   "https://muallim.test/cancelled",
		// The IPN names the workspace and the order: one URL is registered in the
		// merchant panel, and the tables behind it are filtered by tenant.
		"ipn_url": "https://api.muallim.test/v1/payments/sslcommerz/ipn/" +
			order.TenantID.String() + "/" + order.ID.String(),
		"product_profile": "non-physical-goods",
		"shipping_method": "NO",
		"value_a":         order.TenantID.String(),
		"value_b":         order.ID.String(),
	}
	for k, v := range want {
		if got.Get(k) != v {
			t.Errorf("form[%s] = %q, want %q", k, got.Get(k), v)
		}
	}
	for _, k := range []string{"fail_url", "product_name", "product_category", "cus_name", "cus_email", "cus_phone", "cus_add1", "cus_city", "cus_postcode", "cus_country"} {
		if got.Get(k) == "" {
			t.Errorf("form[%s] is empty, and SSLCommerz requires it", k)
		}
	}
}

func TestSSLCommerzCheckoutRefused(t *testing.T) {
	t.Parallel()

	account := sslAccount()
	driver := sslDriver(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"FAILED","failedreason":"Store Credential Error"}`))
	})

	_, _, err := driver.Checkout(context.Background(), account, sslOrder(account, 500, "BDT"), CheckoutURLs{})
	if !errors.Is(err, ErrGatewayUnavailable) {
		t.Fatalf("err = %v, want ErrGatewayUnavailable", err)
	}
}

func TestSSLCommerzCheckoutWithoutCredentials(t *testing.T) {
	t.Parallel()

	driver := sslDriver(t, func(http.ResponseWriter, *http.Request) {
		t.Error("the gateway was called without credentials")
	})

	account := Account{ID: uuid.New(), TenantID: uuid.New(), Gateway: GatewaySSLCommerz}
	_, _, err := driver.Checkout(context.Background(), account, sslOrder(account, 500, "BDT"), CheckoutURLs{})
	if !errors.Is(err, ErrCredentials) {
		t.Fatalf("err = %v, want ErrCredentials", err)
	}
}

func TestSSLCommerzIPNHash(t *testing.T) {
	t.Parallel()

	account := sslAccount()
	order := sslOrder(account, 120_000, "BDT")
	signed := signIPN(t, sslIPN(order, "val_1", "bank_1"), account.Credentials.Secret)

	if err := verifyIPNHash(signed, account.Credentials.Secret); err != nil {
		t.Fatalf("a correctly signed ipn was refused: %v", err)
	}

	tampered := make(url.Values, len(signed))
	for k, v := range signed {
		tampered[k] = v
	}
	tampered.Set("amount", "1.00")
	if err := verifyIPNHash(tampered, account.Credentials.Secret); !errors.Is(err, ErrSignature) {
		t.Errorf("tampered amount: err = %v, want ErrSignature", err)
	}

	if err := verifyIPNHash(signed, "the-wrong-password"); !errors.Is(err, ErrSignature) {
		t.Errorf("wrong store password: err = %v, want ErrSignature", err)
	}

	unsigned := sslIPN(order, "val_1", "bank_1")
	if err := verifyIPNHash(unsigned, account.Credentials.Secret); !errors.Is(err, ErrSignature) {
		t.Errorf("unsigned ipn: err = %v, want ErrSignature", err)
	}
}

func TestSSLCommerzConfirm(t *testing.T) {
	t.Parallel()

	account := sslAccount()
	order := sslOrder(account, 120_000, "BDT")

	driver := sslDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/validator/api/validationserverAPI.php" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("val_id"); got != "val_1" {
			t.Errorf("val_id = %q", got)
		}
		_, _ = w.Write([]byte(`{"status":"VALID","tran_id":"` + order.ID.String() +
			`","val_id":"val_1","amount":"1200.00","currency":"BDT","bank_tran_id":"bank_1"}`))
	})

	signed := signIPN(t, sslIPN(order, "val_1", "bank_1"), account.Credentials.Secret)
	event, err := driver.Confirm(context.Background(), account, order, toMap(signed))
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if event.Kind != EventPaid {
		t.Errorf("kind = %q, want paid", event.Kind)
	}
	if event.ExternalID != order.ID.String() {
		t.Errorf("externalID = %q", event.ExternalID)
	}
	// A refund is issued against the bank transaction, not the session.
	if event.PaymentExternalID != "bank_1" {
		t.Errorf("paymentExternalID = %q, want bank_1", event.PaymentExternalID)
	}
}

func TestSSLCommerzConfirmInvalidTransaction(t *testing.T) {
	t.Parallel()

	account := sslAccount()
	order := sslOrder(account, 120_000, "BDT")

	driver := sslDriver(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"INVALID_TRANSACTION","error":"no transaction"}`))
	})

	signed := signIPN(t, sslIPN(order, "val_1", "bank_1"), account.Credentials.Secret)
	_, err := driver.Confirm(context.Background(), account, order, toMap(signed))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSSLCommerzConfirmAmountMismatch(t *testing.T) {
	t.Parallel()

	account := sslAccount()
	order := sslOrder(account, 120_000, "BDT")

	// A signed IPN, validated by SSLCommerz — but for one taka, not twelve hundred.
	driver := sslDriver(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"VALID","tran_id":"` + order.ID.String() +
			`","val_id":"val_1","amount":"1.00","currency":"BDT","bank_tran_id":"bank_1"}`))
	})

	ipn := sslIPN(order, "val_1", "bank_1")
	ipn.Set("amount", "1.00")
	signed := signIPN(t, ipn, account.Credentials.Secret)

	_, err := driver.Confirm(context.Background(), account, order, toMap(signed))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSSLCommerzConfirmForeignCurrency(t *testing.T) {
	t.Parallel()

	account := sslAccount()
	order := sslOrder(account, 4_999, "USD") // 49.99 USD

	// For a non-BDT sale the settled amount is converted taka; currency_amount is what
	// we asked for, and is what the order must be checked against.
	driver := sslDriver(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"VALIDATED","tran_id":"` + order.ID.String() +
			`","val_id":"val_1","amount":"5998.80","currency":"BDT","currency_type":"USD",` +
			`"currency_amount":"49.99","bank_tran_id":"bank_2"}`))
	})

	signed := signIPN(t, sslIPN(order, "val_1", "bank_2"), account.Credentials.Secret)
	event, err := driver.Confirm(context.Background(), account, order, toMap(signed))
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if event.Kind != EventPaid {
		t.Errorf("kind = %q, want paid", event.Kind)
	}
}

func TestSSLCommerzConfirmFailed(t *testing.T) {
	t.Parallel()

	account := sslAccount()
	order := sslOrder(account, 120_000, "BDT")

	driver := sslDriver(t, func(http.ResponseWriter, *http.Request) {
		t.Error("a failed ipn must not reach the validation api")
	})

	ipn := sslIPN(order, "", "")
	ipn.Set("status", "FAILED")
	signed := signIPN(t, ipn, account.Credentials.Secret)

	event, err := driver.Confirm(context.Background(), account, order, toMap(signed))
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if event.Kind != EventFailed {
		t.Errorf("kind = %q, want failed", event.Kind)
	}
}

func TestSSLCommerzConfirmOtherOrder(t *testing.T) {
	t.Parallel()

	account := sslAccount()
	order := sslOrder(account, 120_000, "BDT")
	other := sslOrder(account, 120_000, "BDT")

	driver := sslDriver(t, func(http.ResponseWriter, *http.Request) {
		t.Error("an ipn for another order must not reach the validation api")
	})

	signed := signIPN(t, sslIPN(other, "val_1", "bank_1"), account.Credentials.Secret)
	_, err := driver.Confirm(context.Background(), account, order, toMap(signed))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSSLCommerzRefund(t *testing.T) {
	t.Parallel()

	account := sslAccount()
	order := sslOrder(account, 120_000, "BDT")
	order.Status, order.PaymentExternalID = OrderPaid, "bank_1"

	driver := sslDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/validator/api/merchantTransIDvalidationAPI.php" {
			t.Errorf("path = %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("bank_tran_id") != "bank_1" {
			t.Errorf("bank_tran_id = %q", q.Get("bank_tran_id"))
		}
		if q.Get("refund_amount") != "1200.00" {
			t.Errorf("refund_amount = %q, want major units", q.Get("refund_amount"))
		}
		_, _ = w.Write([]byte(`{"APIConnect":"DONE","status":"processing","refund_ref_id":"ref_1"}`))
	})

	refundID, err := driver.Refund(context.Background(), account, order)
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if refundID != "ref_1" {
		t.Errorf("refundID = %q", refundID)
	}
}

func TestSSLCommerzRefundRefused(t *testing.T) {
	t.Parallel()

	account := sslAccount()
	order := sslOrder(account, 120_000, "BDT")
	order.Status, order.PaymentExternalID = OrderPaid, "bank_1"

	driver := sslDriver(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"APIConnect":"DONE","status":"failed","errorReason":"already refunded"}`))
	})

	if _, err := driver.Refund(context.Background(), account, order); !errors.Is(err, ErrGatewayUnavailable) {
		t.Fatalf("err = %v, want ErrGatewayUnavailable", err)
	}
}

func TestSSLCommerzOnboardIsUnsupported(t *testing.T) {
	t.Parallel()

	driver := sslDriver(t, func(http.ResponseWriter, *http.Request) {})

	if _, err := driver.Onboard(context.Background(), uuid.New(), "https://muallim.test/back"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}

	account := sslAccount()
	got, err := driver.AccountStatus(context.Background(), account)
	if err != nil {
		t.Fatalf("AccountStatus: %v", err)
	}
	if !got.Ready() {
		t.Errorf("an account holding credentials is not ready: %+v", got)
	}

	account.Credentials = Credentials{}
	got, err = driver.AccountStatus(context.Background(), account)
	if err != nil {
		t.Fatalf("AccountStatus: %v", err)
	}
	if got.Ready() {
		t.Errorf("an account with no credentials is ready: %+v", got)
	}
}

func TestSSLCommerzMoneyRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		minor int64
		major string
	}{
		{0, "0.00"},
		{5, "0.05"},
		{50, "0.50"},
		{100, "1.00"},
		{999, "9.99"},
		{120_000, "1200.00"},
		{9_999_999, "99999.99"},
		{1, "0.01"},
	}

	for _, c := range cases {
		if got := minorToMajor(c.minor); got != c.major {
			t.Errorf("minorToMajor(%d) = %q, want %q", c.minor, got, c.major)
		}
		got, err := majorToMinor(c.major)
		if err != nil {
			t.Fatalf("majorToMinor(%q): %v", c.major, err)
		}
		if got != c.minor {
			t.Errorf("majorToMinor(%q) = %d, want %d", c.major, got, c.minor)
		}
	}

	// What a gateway actually sends: trailing spaces, one decimal place, none at all.
	loose := []struct {
		in    string
		minor int64
	}{
		{"1200", 120_000},
		{"1200.5", 120_050},
		{" 49.99 ", 4_999},
		{"0.05", 5},
	}
	for _, c := range loose {
		got, err := majorToMinor(c.in)
		if err != nil {
			t.Fatalf("majorToMinor(%q): %v", c.in, err)
		}
		if got != c.minor {
			t.Errorf("majorToMinor(%q) = %d, want %d", c.in, got, c.minor)
		}
	}

	if _, err := majorToMinor("not-money"); err == nil {
		t.Error("majorToMinor accepted a string that is not money")
	}
}

// RefundStatus turns SSLCommerz's three answers into the three RefundStates the
// polling job acts on. `cancelled` is the one that must not read as done — it is a
// learner out the course and the money.
func TestSSLCommerzRefundStatus(t *testing.T) {
	t.Parallel()

	for status, want := range map[string]RefundState{
		"refunded":   RefundDone,
		"processing": RefundPending,
		"cancelled":  RefundFailed,
		"anything":   RefundPending, // an answer we do not know is not "done"
	} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()

			account := sslAccount()
			order := sslOrder(account, 120_000, "BDT")
			order.Status, order.RefundExternalID = OrderRefunded, "ref_1"

			driver := sslDriver(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("refund_ref_id") != "ref_1" {
					t.Errorf("refund_ref_id = %q", r.URL.Query().Get("refund_ref_id"))
				}
				_, _ = w.Write([]byte(`{"APIConnect":"DONE","status":"` + status + `"}`))
			})

			got, err := driver.RefundStatus(context.Background(), account, order)
			if err != nil {
				t.Fatalf("RefundStatus: %v", err)
			}
			if got != want {
				t.Errorf("status %q → %q, want %q", status, got, want)
			}
		})
	}
}
