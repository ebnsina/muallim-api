package commerce

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// bkashStub is a bKash that answers whatever the test tells it to, and counts what it
// was asked. The grant count is the one that matters: bKash blocks a merchant that
// asks twice in an hour.
type bkashStub struct {
	grants   atomic.Int64
	creates  atomic.Int64
	executes atomic.Int64
	queries  atomic.Int64
	refunds  atomic.Int64

	lastCreate map[string]string
	lastAuth   string
	lastAppKey string

	execute func(w http.ResponseWriter)
	query   func(w http.ResponseWriter)
	refund  func(w http.ResponseWriter)
}

func (s *bkashStub) handler(t *testing.T) http.Handler {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/v1.2.0-beta/tokenized/checkout/token/grant", func(w http.ResponseWriter, r *http.Request) {
		s.grants.Add(1)
		if r.Header.Get("username") != "muallim" || r.Header.Get("password") != "sesame" {
			t.Errorf("grant: username/password headers = %q/%q", r.Header.Get("username"), r.Header.Get("password"))
		}
		writeJSON(t, w, map[string]any{"id_token": "tok_1", "token_type": "Bearer", "expires_in": 3600, "refresh_token": "ref_1"})
	})

	mux.HandleFunc("/v1.2.0-beta/tokenized/checkout/create", func(w http.ResponseWriter, r *http.Request) {
		s.creates.Add(1)
		s.lastAuth = r.Header.Get("Authorization")
		s.lastAppKey = r.Header.Get("X-App-Key")
		s.lastCreate = readJSON[map[string]string](t, r.Body)
		writeJSON(t, w, map[string]any{
			"statusCode": "0000", "statusMessage": "Successful",
			"paymentID": "pay_1", "bkashURL": "https://bkash.test/pay/pay_1",
		})
	})

	mux.HandleFunc("/v1.2.0-beta/tokenized/checkout/execute", func(w http.ResponseWriter, r *http.Request) {
		s.executes.Add(1)
		if s.execute != nil {
			s.execute(w)
			return
		}
		writeJSON(t, w, map[string]any{"statusCode": "0000", "paymentID": "pay_1", "trxID": "TRX1", "transactionStatus": "Completed"})
	})

	mux.HandleFunc("/v1.2.0-beta/tokenized/checkout/payment/status", func(w http.ResponseWriter, r *http.Request) {
		s.queries.Add(1)
		if s.query != nil {
			s.query(w)
			return
		}
		writeJSON(t, w, map[string]any{"statusCode": "0000", "paymentID": "pay_1", "transactionStatus": "Initiated"})
	})

	mux.HandleFunc("/v2/tokenized-checkout/refund/payment/transaction", func(w http.ResponseWriter, r *http.Request) {
		s.refunds.Add(1)
		if s.refund != nil {
			s.refund(w)
			return
		}
		writeJSON(t, w, map[string]any{"refundTrxId": "REF1", "refundTransactionStatus": "Completed"})
	})

	return mux
}

func writeJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("encode stub response: %v", err)
	}
}

func readJSON[T any](t *testing.T, r io.Reader) T {
	t.Helper()
	var body T
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return body
}

// newBkashTest points the driver at a stub, and hands back the account whose sealed
// secret carries the three credentials that do not fit in Credentials.
func newBkashTest(t *testing.T, stub *bkashStub) (*Bkash, Account) {
	t.Helper()

	server := httptest.NewServer(stub.handler(t))
	t.Cleanup(server.Close)

	driver := NewBkash(true, "https://muallim.test/pay/bkash/callback", server.Client())
	driver.host = server.URL

	secret, err := json.Marshal(bkashSecret{AppSecret: "app_secret", Username: "muallim", Password: "sesame"})
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}

	account := Account{
		ID: uuid.New(), TenantID: uuid.New(), Gateway: GatewayBkash,
		Status: AccountActive, ChargesEnabled: true,
		Credentials: Credentials{PublicID: "app_key", Secret: string(secret)},
	}
	return driver, account
}

func bkashOrder(price Money) Order {
	return Order{
		ID: uuid.New(), TenantID: uuid.New(), CourseID: uuid.New(), UserID: uuid.New(),
		Price: price, Status: OrderPending, Gateway: GatewayBkash, ExternalID: "pay_1",
	}
}

// The token is granted once and reused. bKash locks the merchant out for an hour if it
// is not, so this is the assertion the file exists for.
func TestBkashTokenIsGrantedOnceAndReused(t *testing.T) {
	t.Parallel()

	stub := &bkashStub{}
	driver, account := newBkashTest(t, stub)

	for range 2 {
		if _, _, err := driver.Checkout(t.Context(), account, bkashOrder(Money{AmountMinor: 50_000, Currency: "BDT"}), CheckoutURLs{}); err != nil {
			t.Fatalf("checkout: %v", err)
		}
	}

	if got := stub.grants.Load(); got != 1 {
		t.Errorf("token grants = %d, want 1", got)
	}
	if got := stub.creates.Load(); got != 2 {
		t.Errorf("creates = %d, want 2", got)
	}
	if stub.lastAuth != "tok_1" {
		t.Errorf("Authorization = %q, want the bare token", stub.lastAuth)
	}
	if stub.lastAppKey != "app_key" {
		t.Errorf("X-App-Key = %q", stub.lastAppKey)
	}
}

func TestBkashCheckout(t *testing.T) {
	t.Parallel()

	stub := &bkashStub{}
	driver, account := newBkashTest(t, stub)
	order := bkashOrder(Money{AmountMinor: 123_456, Currency: "BDT"})

	url, external, err := driver.Checkout(t.Context(), account, order, CheckoutURLs{})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if url != "https://bkash.test/pay/pay_1" || external != "pay_1" {
		t.Fatalf("checkout = %q, %q", url, external)
	}

	want := map[string]string{
		"mode":                  "0011",
		"amount":                "1234.56",
		"currency":              "BDT",
		"intent":                "sale",
		"merchantInvoiceNumber": order.ID.String(),
		// The callback names the workspace and the order in path segments: bKash appends
		// its own query string, and the tables behind it are filtered by tenant.
		"callbackURL": "https://muallim.test/pay/bkash/callback/" +
			order.TenantID.String() + "/" + order.ID.String(),
	}
	for key, value := range want {
		if stub.lastCreate[key] != value {
			t.Errorf("create %s = %q, want %q", key, stub.lastCreate[key], value)
		}
	}
	if stub.lastCreate["payerReference"] != order.UserID.String() {
		t.Errorf("create payerReference = %q", stub.lastCreate["payerReference"])
	}
}

func TestBkashRefusesForeignCurrency(t *testing.T) {
	t.Parallel()

	stub := &bkashStub{}
	driver, account := newBkashTest(t, stub)

	_, _, err := driver.Checkout(t.Context(), account, bkashOrder(Money{AmountMinor: 2000, Currency: "USD"}), CheckoutURLs{})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("checkout USD = %v, want ErrUnsupported", err)
	}
	if stub.creates.Load() != 0 {
		t.Errorf("a non-BDT order reached bkash")
	}
}

func TestBkashCredentialsMustParse(t *testing.T) {
	t.Parallel()

	stub := &bkashStub{}
	driver, account := newBkashTest(t, stub)
	account.Credentials.Secret = "not json"

	_, _, err := driver.Checkout(t.Context(), account, bkashOrder(Money{AmountMinor: 2000, Currency: "BDT"}), CheckoutURLs{})
	if !errors.Is(err, ErrCredentials) {
		t.Fatalf("checkout with an unparseable secret = %v, want ErrCredentials", err)
	}
}

func TestBkashConfirm(t *testing.T) {
	t.Parallel()

	order := bkashOrder(Money{AmountMinor: 50_000, Currency: "BDT"})
	invoice := order.ID.String()

	completed := func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"statusCode":"0000","paymentID":"pay_1","trxID":"TRX1","transactionStatus":"Completed",` +
			`"amount":"500.00","currency":"BDT","merchantInvoiceNumber":"` + invoice + `"}`))
	}

	tests := []struct {
		name     string
		query    map[string]string
		execute  func(w http.ResponseWriter)
		want     EventKind
		wantErr  error
		wantTrx  string
		executes int64
		queries  int64
	}{
		{
			name:  "failure never executes",
			query: map[string]string{"paymentID": "pay_1", "status": "failure"},
			want:  EventFailed,
		},
		{
			name:  "cancel never executes",
			query: map[string]string{"paymentID": "pay_1", "status": "cancel"},
			want:  EventFailed,
		},
		{
			name:     "completed pays",
			query:    map[string]string{"paymentID": "pay_1", "status": "success"},
			execute:  completed,
			want:     EventPaid,
			wantTrx:  "TRX1",
			executes: 1,
		},
		{
			name:  "another order's invoice is refused",
			query: map[string]string{"paymentID": "pay_1", "status": "success"},
			execute: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte(`{"statusCode":"0000","trxID":"TRX1","transactionStatus":"Completed",` +
					`"amount":"500.00","merchantInvoiceNumber":"` + uuid.New().String() + `"}`))
			},
			wantErr:  ErrNotFound,
			executes: 1,
		},
		{
			name:  "a different amount is refused",
			query: map[string]string{"paymentID": "pay_1", "status": "success"},
			execute: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte(`{"statusCode":"0000","trxID":"TRX1","transactionStatus":"Completed",` +
					`"amount":"5.00","merchantInvoiceNumber":"` + invoice + `"}`))
			},
			wantErr:  ErrNotFound,
			executes: 1,
		},
		{
			name:    "a payment for another order is refused",
			query:   map[string]string{"paymentID": "pay_other", "status": "success"},
			wantErr: ErrNotFound,
		},
		{
			name:  "an execute that answers nothing falls back to query",
			query: map[string]string{"paymentID": "pay_1", "status": "success"},
			execute: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusGatewayTimeout)
			},
			// Initiated: the learner never paid, and execute must not be retried.
			want:     EventFailed,
			executes: 1,
			queries:  1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			stub := &bkashStub{execute: test.execute}
			driver, account := newBkashTest(t, stub)

			event, err := driver.Confirm(t.Context(), account, order, test.query)
			switch {
			case test.wantErr != nil:
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("confirm = %v, want %v", err, test.wantErr)
				}
			case err != nil:
				t.Fatalf("confirm: %v", err)
			case event.Kind != test.want:
				t.Fatalf("kind = %q, want %q", event.Kind, test.want)
			case event.PaymentExternalID != test.wantTrx:
				t.Fatalf("trxID = %q, want %q", event.PaymentExternalID, test.wantTrx)
			case test.want == EventPaid && (event.OrderID != order.ID || event.TenantID != order.TenantID || event.ExternalID != "pay_1"):
				t.Fatalf("event = %+v", event)
			}

			if got := stub.executes.Load(); got != test.executes {
				t.Errorf("executes = %d, want %d", got, test.executes)
			}
			if got := stub.queries.Load(); got != test.queries {
				t.Errorf("queries = %d, want %d", got, test.queries)
			}
		})
	}
}

// A query answering Completed is a payment execute lost the answer to, and it settles.
func TestBkashQueryRecoversACompletedPayment(t *testing.T) {
	t.Parallel()

	order := bkashOrder(Money{AmountMinor: 50_000, Currency: "BDT"})
	stub := &bkashStub{
		execute: func(w http.ResponseWriter) { w.WriteHeader(http.StatusGatewayTimeout) },
		query: func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"statusCode":"0000","trxID":"TRX9","transactionStatus":"Completed",` +
				`"amount":"500.00","merchantInvoiceNumber":"` + order.ID.String() + `"}`))
		},
	}
	driver, account := newBkashTest(t, stub)

	event, err := driver.Confirm(t.Context(), account, order, map[string]string{"paymentID": "pay_1", "status": "success"})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if event.Kind != EventPaid || event.PaymentExternalID != "TRX9" {
		t.Fatalf("event = %+v, want paid TRX9", event)
	}
	if stub.executes.Load() != 1 {
		t.Errorf("execute was retried: %d calls", stub.executes.Load())
	}
}

func TestBkashRefund(t *testing.T) {
	t.Parallel()

	stub := &bkashStub{}
	driver, account := newBkashTest(t, stub)

	order := bkashOrder(Money{AmountMinor: 50_000, Currency: "BDT"})
	order.PaymentExternalID = "TRX1"

	refund, err := driver.Refund(t.Context(), account, order)
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if refund != "REF1" {
		t.Fatalf("refund = %q, want REF1", refund)
	}

	order.PaymentExternalID = ""
	if _, err := driver.Refund(t.Context(), account, order); !errors.Is(err, ErrNotFound) {
		t.Fatalf("refund with no trxID = %v, want ErrNotFound", err)
	}
}

// The v2 error envelope names its fields differently, and a refund that failed under it
// must not read as one that worked.
func TestBkashRefundReadsTheV2ErrorEnvelope(t *testing.T) {
	t.Parallel()

	stub := &bkashStub{refund: func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"internalCode":"2006","externalCode":"1001","errorMessageEn":"Refund window has passed"}`))
	}}
	driver, account := newBkashTest(t, stub)

	order := bkashOrder(Money{AmountMinor: 50_000, Currency: "BDT"})
	order.PaymentExternalID = "TRX1"

	_, err := driver.Refund(t.Context(), account, order)
	if !errors.Is(err, ErrGatewayUnavailable) {
		t.Fatalf("refund = %v, want ErrGatewayUnavailable", err)
	}
}

func TestBkashOnboardAndAccountStatus(t *testing.T) {
	t.Parallel()

	stub := &bkashStub{}
	driver, account := newBkashTest(t, stub)

	if _, err := driver.Onboard(t.Context(), account.TenantID, "https://muallim.test/return"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("onboard = %v, want ErrUnsupported", err)
	}

	ready, err := driver.AccountStatus(t.Context(), account)
	if err != nil {
		t.Fatalf("account status: %v", err)
	}
	if !ready.Ready() {
		t.Errorf("an account with credentials is not ready: %+v", ready)
	}

	account.Credentials = Credentials{}
	blank, err := driver.AccountStatus(t.Context(), account)
	if err != nil {
		t.Fatalf("account status: %v", err)
	}
	if blank.Ready() {
		t.Errorf("an account with no credentials is ready")
	}
}

func TestBkashMoneyConversion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		minor int64
		major string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{50, "0.50"},
		{100, "1.00"},
		{999, "9.99"},
		{1050, "10.50"},
		{123_456, "1234.56"},
		{100_000_000, "1000000.00"},
	}

	for _, test := range tests {
		if got := bkashMajor(test.minor); got != test.major {
			t.Errorf("bkashMajor(%d) = %q, want %q", test.minor, got, test.major)
		}
		got, err := bkashMinor(test.major)
		if err != nil {
			t.Fatalf("bkashMinor(%q): %v", test.major, err)
		}
		if got != test.minor {
			t.Errorf("bkashMinor(%q) = %d, want %d", test.major, got, test.minor)
		}
	}

	// bKash writes an amount more than one way, and some of them are not amounts.
	loose := map[string]int64{"500": 50_000, "500.5": 50_050, " 12.34 ": 1234}
	for text, want := range loose {
		got, err := bkashMinor(text)
		if err != nil || got != want {
			t.Errorf("bkashMinor(%q) = %d, %v, want %d", text, got, err, want)
		}
	}
	for _, text := range []string{"", "abc", "1.234", "-5.00", "+5.00", "1,234.00", "12.3a"} {
		if _, err := bkashMinor(text); err == nil {
			t.Errorf("bkashMinor(%q) was accepted", text)
		}
	}
}

// A token past its life is granted again rather than sent stale.
func TestBkashTokenRefreshesWhenItExpires(t *testing.T) {
	t.Parallel()

	stub := &bkashStub{}
	driver, account := newBkashTest(t, stub)
	order := bkashOrder(Money{AmountMinor: 50_000, Currency: "BDT"})

	if _, _, err := driver.Checkout(t.Context(), account, order, CheckoutURLs{}); err != nil {
		t.Fatalf("checkout: %v", err)
	}

	entry, ok := driver.tokens.Load("app_key")
	if !ok {
		t.Fatal("no token was cached")
	}
	cached, ok := entry.(*bkashToken)
	if !ok {
		t.Fatalf("token cache holds %T", entry)
	}
	cached.mu.Lock()
	cached.expires = time.Now().Add(-time.Minute)
	cached.mu.Unlock()

	if _, _, err := driver.Checkout(t.Context(), account, order, CheckoutURLs{}); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if got := stub.grants.Load(); got != 2 {
		t.Errorf("grants = %d, want 2 (one per expiry)", got)
	}
}
