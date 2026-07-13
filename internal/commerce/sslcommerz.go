package commerce

import (
	"context"
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SSLCommerz's two hosts. A deployment picks one; a test points at its own.
const (
	SSLCommerzSandbox = "https://sandbox.sslcommerz.com"
	SSLCommerzLive    = "https://securepay.sslcommerz.com"
)

/*
SSLCommerz is the driver for SSLCommerz, Bangladesh's card and mobile-banking
gateway.

There is no platform account and no OAuth: a school signs up with SSLCommerz itself
and is its own merchant, so every call carries that school's store id and store
password, handed to the driver by the service and held nowhere else.

It settles through Confirm, not through a webhook, and that is deliberate. The IPN's
hash is keyed on the store password, so nothing can authenticate an IPN without
knowing which workspace sent it — and a Webhooker is handed a payload and a
signature and no account at all. Confirm has the account, so Confirm can check the
hash, and then does what the docs require anyway: asks the Order Validation API,
which is the authority, and compares what it says against the order we wrote.
*/
type SSLCommerz struct {
	baseURL string

	// ipnURL is where SSLCommerz posts, registered in the merchant panel. The same URL
	// for every workspace: the IPN names its tenant in value_a.
	ipnURL string

	http *http.Client
}

/*
NewSSLCommerz builds the driver.

The host is a parameter rather than a `sandbox bool` because it is the one seam a
test needs — cmd/ passes SSLCommerzSandbox or SSLCommerzLive, and a test passes
httptest's own URL.
*/
func NewSSLCommerz(baseURL, ipnURL string, client *http.Client) (*SSLCommerz, error) {
	if _, err := url.ParseRequestURI(baseURL); err != nil {
		return nil, fmt.Errorf("commerce: sslcommerz base url: %w", err)
	}
	if _, err := url.ParseRequestURI(ipnURL); err != nil {
		return nil, fmt.Errorf("commerce: sslcommerz ipn url: %w", err)
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &SSLCommerz{baseURL: strings.TrimSuffix(baseURL, "/"), ipnURL: ipnURL, http: client}, nil
}

var (
	_ Gateway   = (*SSLCommerz)(nil)
	_ Confirmer = (*SSLCommerz)(nil)
	_ Refunder  = (*SSLCommerz)(nil)
)

// Name is the gateway this driver serves.
func (*SSLCommerz) Name() string { return GatewaySSLCommerz }

// Onboard refuses: SSLCommerz has no onboarding flow to send an author through. The
// school's store id and password arrive through Service.SetCredentials instead.
func (*SSLCommerz) Onboard(context.Context, uuid.UUID, string) (Onboarding, error) {
	return Onboarding{}, fmt.Errorf("%w: sslcommerz credentials are pasted, not granted", ErrUnsupported)
}

// AccountStatus has nothing to ask: SSLCommerz publishes no account API, and an
// account with credentials is an account that can be charged against.
func (*SSLCommerz) AccountStatus(_ context.Context, account Account) (Account, error) {
	account.Status, account.ChargesEnabled = AccountPending, false
	if account.Credentials.Has() {
		account.Status, account.ChargesEnabled = AccountActive, true
	}
	return account, nil
}

// sslInitResponse is the initiate call's reply, reduced to what we act on.
type sslInitResponse struct {
	Status         string `json:"status"`
	FailedReason   string `json:"failedreason"`
	SessionKey     string `json:"sessionkey"`
	GatewayPageURL string `json:"GatewayPageURL"`
}

// Checkout opens a hosted session on the school's own store.
func (s *SSLCommerz) Checkout(ctx context.Context, account Account, order Order, urls CheckoutURLs) (string, string, error) {
	if !account.Credentials.Has() {
		return "", "", fmt.Errorf("%w: sslcommerz needs a store id and password", ErrCredentials)
	}

	// tran_id is the order id, and it is what the IPN echoes back: the session key is
	// not, so it cannot be the key the (gateway, external_id) index dedupes on.
	tranID := order.ID.String()

	form := url.Values{
		"store_id":         {account.Credentials.PublicID},
		"store_passwd":     {account.Credentials.Secret},
		"total_amount":     {minorToMajor(order.Price.AmountMinor)},
		"currency":         {order.Price.Currency},
		"tran_id":          {tranID},
		"success_url":      {urls.Success},
		"fail_url":         {urls.Cancel},
		"cancel_url":       {urls.Cancel},
		"ipn_url":          {s.ipnURL + "/" + order.TenantID.String() + "/" + order.ID.String()},
		"product_name":     {"Course " + order.CourseID.String()},
		"product_category": {"course"},
		"product_profile":  {"non-physical-goods"},
		"shipping_method":  {"NO"},

		// The order carries no learner contact details and this package may not go and
		// look for them; SSLCommerz requires the keys, not their truth.
		"cus_name":     {"Learner"},
		"cus_email":    {order.UserID.String() + "@learner.invalid"},
		"cus_phone":    {"0000000000"},
		"cus_add1":     {"N/A"},
		"cus_city":     {"Dhaka"},
		"cus_postcode": {"1000"},
		"cus_country":  {"Bangladesh"},

		"value_a": {order.TenantID.String()},
		"value_b": {order.ID.String()},
	}

	var out sslInitResponse
	if err := s.postForm(ctx, "/gwprocess/v4/api.php", form, &out); err != nil {
		return "", "", err
	}
	if !strings.EqualFold(out.Status, "SUCCESS") || out.GatewayPageURL == "" {
		return "", "", fmt.Errorf("%w: sslcommerz refused the session: %s", ErrGatewayUnavailable, out.FailedReason)
	}
	return out.GatewayPageURL, tranID, nil
}

// sslValidationResponse is the Order Validation API's reply.
type sslValidationResponse struct {
	Status         string    `json:"status"`
	TranID         string    `json:"tran_id"`
	ValID          string    `json:"val_id"`
	Amount         numString `json:"amount"`
	Currency       string    `json:"currency"`
	CurrencyType   string    `json:"currency_type"`
	CurrencyAmount numString `json:"currency_amount"`
	BankTranID     string    `json:"bank_tran_id"`
	Error          string    `json:"error"`
}

/*
Confirm settles an SSLCommerz order, and is the only path that does.

Three checks, in order: the IPN's MD5 hash against the store password (this request
came from SSLCommerz), the Order Validation API (SSLCommerz agrees the money moved —
the docs make this mandatory and it is the authority even when the IPN arrived), and
the validated amount against the price we wrote (nobody edited the total on the way).
*/
func (s *SSLCommerz) Confirm(ctx context.Context, account Account, order Order, query map[string]string) (Event, error) {
	if !account.Credentials.Has() {
		return Event{}, fmt.Errorf("%w: sslcommerz needs a store id and password", ErrCredentials)
	}

	fields := url.Values{}
	for k, v := range query {
		fields.Set(k, v)
	}

	if err := verifyIPNHash(fields, account.Credentials.Secret); err != nil {
		return Event{}, err
	}

	// The hash proves the message; it does not prove it is about *this* order.
	if tran := fields.Get("tran_id"); tran != order.ID.String() {
		return Event{}, fmt.Errorf("%w: the ipn names order %q", ErrNotFound, tran)
	}

	switch strings.ToUpper(fields.Get("status")) {
	case "VALID", "VALIDATED":
	case "FAILED", "CANCELLED", "EXPIRED":
		return Event{Kind: EventFailed, ExternalID: order.ID.String()}, nil
	default:
		return Event{Kind: EventIgnored, ExternalID: order.ID.String()}, nil
	}

	valID := fields.Get("val_id")
	if valID == "" {
		return Event{}, fmt.Errorf("%w: a valid ipn carries a val_id", ErrSignature)
	}

	q := url.Values{
		"val_id":       {valID},
		"store_id":     {account.Credentials.PublicID},
		"store_passwd": {account.Credentials.Secret},
		"format":       {"json"},
		"v":            {"1"},
	}
	var out sslValidationResponse
	if err := s.get(ctx, "/validator/api/validationserverAPI.php", q, &out); err != nil {
		return Event{}, err
	}

	switch strings.ToUpper(out.Status) {
	case "VALID", "VALIDATED":
	default:
		return Event{}, fmt.Errorf("%w: sslcommerz validated %s as %s %s", ErrNotFound, valID, out.Status, out.Error)
	}
	if out.TranID != order.ID.String() {
		return Event{}, fmt.Errorf("%w: validation names order %q", ErrNotFound, out.TranID)
	}

	if err := s.checkAmount(out, order.Price); err != nil {
		return Event{}, err
	}

	return Event{
		Kind:              EventPaid,
		ExternalID:        order.ID.String(),
		PaymentExternalID: out.BankTranID,
	}, nil
}

// checkAmount compares what SSLCommerz settled against what the course costs. For a
// non-BDT sale `amount`/`currency` are the converted taka and currency_amount/_type
// are what we sent, so those are what an order in USD must be checked against.
func (s *SSLCommerz) checkAmount(out sslValidationResponse, price Money) error {
	paid, currency := string(out.Amount), out.Currency
	if out.CurrencyAmount != "" && out.CurrencyType != "" {
		paid, currency = string(out.CurrencyAmount), out.CurrencyType
	}

	minor, err := majorToMinor(paid)
	if err != nil {
		return fmt.Errorf("%w: sslcommerz settled an amount we cannot read: %v", ErrNotFound, err)
	}
	if minor != price.AmountMinor || !strings.EqualFold(currency, price.Currency) {
		return fmt.Errorf("%w: sslcommerz settled %s %s against a price of %d %s",
			ErrNotFound, paid, currency, price.AmountMinor, price.Currency)
	}
	return nil
}

// sslRefundResponse is the refund API's reply.
type sslRefundResponse struct {
	APIConnect  string `json:"APIConnect"`
	Status      string `json:"status"`
	RefundRefID string `json:"refund_ref_id"`
	Errors      string `json:"errorReason"`
}

// Refund gives the money back against the bank transaction the payment settled on.
// SSLCommerz refunds asynchronously: `processing` is an accepted refund, not a
// pending call, and the refund_ref_id is how it is chased.
func (s *SSLCommerz) Refund(ctx context.Context, account Account, order Order) (string, error) {
	if !account.Credentials.Has() {
		return "", fmt.Errorf("%w: sslcommerz needs a store id and password", ErrCredentials)
	}
	if order.PaymentExternalID == "" {
		return "", fmt.Errorf("%w: the order has no bank transaction to refund", ErrNotPaid)
	}

	q := url.Values{
		"bank_tran_id":   {order.PaymentExternalID},
		"refund_amount":  {minorToMajor(order.Price.AmountMinor)},
		"refund_remarks": {"Refund for order " + order.ID.String()},
		"store_id":       {account.Credentials.PublicID},
		"store_passwd":   {account.Credentials.Secret},
		"format":         {"json"},
		"v":              {"1"},
	}
	var out sslRefundResponse
	if err := s.get(ctx, "/validator/api/merchantTransIDvalidationAPI.php", q, &out); err != nil {
		return "", err
	}

	ok := strings.EqualFold(out.APIConnect, "DONE") &&
		(strings.EqualFold(out.Status, "success") || strings.EqualFold(out.Status, "processing"))
	if !ok {
		return "", fmt.Errorf("%w: sslcommerz refused the refund: %s %s %s",
			ErrGatewayUnavailable, out.APIConnect, out.Status, out.Errors)
	}
	return out.RefundRefID, nil
}

// ---------------------------------------------------------------- the hash

/*
verifyIPNHash checks an IPN against the store password, exactly as the docs specify:
take the field names listed in verify_key, pair each with its raw posted value, add
store_passwd as the lowercase hex md5 of the password, sort the names byte-ascending,
join as k1=v1&k2=v2, and md5 that.

MD5 because SSLCommerz says MD5. It is not our choice, and the secret in it is the
only thing that makes it a signature at all.
*/
func verifyIPNHash(fields url.Values, storePassword string) error {
	sign, key := fields.Get("verify_sign"), fields.Get("verify_key")
	if sign == "" || key == "" {
		return fmt.Errorf("%w: the ipn carries no verify_sign", ErrSignature)
	}

	digest := md5.Sum([]byte(storePassword))
	values := map[string]string{"store_passwd": hex.EncodeToString(digest[:])}
	names := []string{"store_passwd"}

	for _, name := range strings.Split(key, ",") {
		name = strings.TrimSpace(name)
		if name == "" || !fields.Has(name) {
			continue
		}
		if _, seen := values[name]; seen {
			continue
		}
		values[name] = fields.Get(name)
		names = append(names, name)
	}

	// Sorted by name, not by pair: a byte sort of "k=v" strings orders "a=1" after
	// "a1=2", because '=' is 0x3d and '1' is 0x31.
	sort.Strings(names)

	pairs := make([]string, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, name+"="+values[name])
	}

	want := md5.Sum([]byte(strings.Join(pairs, "&")))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(want[:])), []byte(strings.ToLower(sign))) != 1 {
		return fmt.Errorf("%w: sslcommerz ipn hash", ErrSignature)
	}
	return nil
}

// ---------------------------------------------------------------- money

// minorToMajor renders minor units as SSLCommerz's decimal(10,2). Integer maths, and
// never a float: binary cannot hold 0.1, and the penny it drops is somebody's.
func minorToMajor(minor int64) string {
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	return fmt.Sprintf("%s%d.%02d", sign, minor/100, minor%100)
}

// majorToMinor parses a gateway's decimal string into minor units, by text, for the
// same reason.
func majorToMinor(major string) (int64, error) {
	s := strings.TrimSpace(major)
	if s == "" {
		return 0, fmt.Errorf("commerce: empty amount")
	}

	whole, fraction, _ := strings.Cut(s, ".")
	units, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("commerce: amount %q: %w", major, err)
	}

	fraction = (fraction + "00")[:2]
	cents, err := strconv.ParseInt(fraction, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("commerce: amount %q: %w", major, err)
	}

	if strings.HasPrefix(whole, "-") {
		return units*100 - cents, nil
	}
	return units*100 + cents, nil
}

// numString is a JSON field SSLCommerz sends as a string in some replies and as a
// bare number in others.
type numString string

func (n *numString) UnmarshalJSON(b []byte) error {
	*n = numString(strings.Trim(string(b), `"`))
	if *n == "null" {
		*n = ""
	}
	return nil
}

// ---------------------------------------------------------------- transport

func (s *SSLCommerz) postForm(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("commerce: sslcommerz %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return s.do(req, path, out)
}

func (s *SSLCommerz) get(ctx context.Context, path string, query url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+path+"?"+query.Encode(), nil)
	if err != nil {
		return fmt.Errorf("commerce: sslcommerz %s: %w", path, err)
	}
	return s.do(req, path, out)
}

func (s *SSLCommerz) do(req *http.Request, path string, out any) error {
	req.Header.Set("Accept", "application/json")

	res, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: sslcommerz %s: %v", ErrGatewayUnavailable, path, err)
	}
	defer res.Body.Close()

	// Bounded, because a gateway is not a trusted source of a response size.
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: sslcommerz %s: %v", ErrGatewayUnavailable, path, err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("%w: sslcommerz %s answered %d", ErrGatewayUnavailable, path, res.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: sslcommerz %s: %v", ErrGatewayUnavailable, path, err)
	}
	return nil
}
