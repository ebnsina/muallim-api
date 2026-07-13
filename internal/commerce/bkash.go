package commerce

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// bKash hosts, and the version segment. Separate, because refund v2 hangs off the
// host without it: {host}/v2/tokenized-checkout/... .
const (
	bkashSandboxHost = "https://tokenized.sandbox.bka.sh"
	bkashLiveHost    = "https://tokenized.pay.bka.sh"
	bkashVersion     = "v1.2.0-beta"

	// bkashOK is the only statusCode that means yes. HTTP 200 on its own means nothing:
	// bKash answers a refused payment with 200 and an errorCode in the body.
	bkashOK = "0000"

	bkashCompleted = "Completed"

	// bkashTimeout is bKash's own requirement for every call they serve.
	bkashTimeout = 30 * time.Second

	// bkashTokenTTL is ten minutes short of the id_token's hour. See Bkash.token.
	bkashTokenTTL = 50 * time.Minute
)

/*
Bkash is the driver for bKash tokenized checkout — Bangladesh, BDT only.

bKash sends no webhook. Settlement is: the learner returns to our callback URL,
the handler hands the query string to Confirm, and Confirm goes back to bKash with
a server-to-server execute. The callback itself proves nothing.
*/
type Bkash struct {
	host         string
	version      string
	callbackBase string
	http         *http.Client

	// tokens is one *bkashToken per app_key: bKash blocks a merchant for an hour if
	// it asks for more than two tokens in one, so the token is cached and reused.
	tokens sync.Map
}

var (
	_ Gateway   = (*Bkash)(nil)
	_ Confirmer = (*Bkash)(nil)
	_ Refunder  = (*Bkash)(nil)
)

// NewBkash builds the driver. callbackBase is a base URL with no query string of
// ours: bKash appends its own ?paymentID=&status=&signature= to whatever it is given.
func NewBkash(sandbox bool, callbackBase string, client *http.Client) *Bkash {
	host := bkashLiveHost
	if sandbox {
		host = bkashSandboxHost
	}
	if client == nil {
		client = &http.Client{Timeout: bkashTimeout}
	}
	return &Bkash{
		host:         host,
		version:      bkashVersion,
		callbackBase: strings.TrimRight(callbackBase, "/"),
		http:         client,
	}
}

// Name is the gateway this driver serves.
func (*Bkash) Name() string { return GatewayBkash }

// Onboard refuses: bKash provisions a merchant itself, offline, and the school
// pastes the four credentials into our settings. There is no OAuth to send them to.
func (*Bkash) Onboard(context.Context, uuid.UUID, string) (Onboarding, error) {
	return Onboarding{}, fmt.Errorf("%w: bkash has no onboarding flow", ErrUnsupported)
}

// AccountStatus has nothing to ask: bKash exposes no account endpoint, so an account
// is ready exactly when the workspace has given us credentials to use.
func (*Bkash) AccountStatus(_ context.Context, account Account) (Account, error) {
	account.Status = AccountPending
	account.ChargesEnabled = false
	if account.Credentials.Has() {
		account.Status = AccountActive
		account.ChargesEnabled = true
	}
	return account, nil
}

/*
bkashSecret is the three credentials that do not fit in Credentials.

bKash needs four values and our store holds two, so PublicID is the app_key and
Secret is the other three packed as JSON — Secret is the half the service seals
with AES-GCM, and it seals a blob as happily as a word.
*/
type bkashSecret struct {
	AppSecret string `json:"app_secret"`
	Username  string `json:"username"`
	Password  string `json:"password"`
}

func bkashCredentials(account Account) (string, bkashSecret, error) {
	if !account.Credentials.Has() {
		return "", bkashSecret{}, fmt.Errorf("%w: bkash needs an app key and a sealed secret", ErrCredentials)
	}

	var secret bkashSecret
	if err := json.Unmarshal([]byte(account.Credentials.Secret), &secret); err != nil {
		return "", bkashSecret{}, fmt.Errorf("commerce: bkash secret is not json: %w: %w", ErrCredentials, err)
	}
	if secret.AppSecret == "" || secret.Username == "" || secret.Password == "" {
		return "", bkashSecret{}, fmt.Errorf("%w: bkash needs app_secret, username and password", ErrCredentials)
	}
	return account.Credentials.PublicID, secret, nil
}

// bkashToken is one cached id_token, and the lock that serialises the merchants
// racing to replace it.
type bkashToken struct {
	mu      sync.Mutex
	value   string
	expires time.Time
}

/*
token returns a cached id_token, granting a new one only when the old has aged out.

This is the load-bearing line of the file. An id_token lives an hour and bKash locks
a merchant out for an hour if it grants more than twice within one — so the token is
held per app_key, refreshed at fifty minutes, and the mutex is held across the grant
so that ten concurrent checkouts on a cold cache make one request, not ten.
*/
func (b *Bkash) token(ctx context.Context, appKey string, secret bkashSecret) (string, error) {
	entry, _ := b.tokens.LoadOrStore(appKey, &bkashToken{})
	cached, ok := entry.(*bkashToken)
	if !ok {
		return "", fmt.Errorf("commerce: bkash token cache holds %T", entry)
	}

	cached.mu.Lock()
	defer cached.mu.Unlock()

	if cached.value != "" && time.Now().Before(cached.expires) {
		return cached.value, nil
	}

	body := map[string]string{"app_key": appKey, "app_secret": secret.AppSecret}
	headers := map[string]string{"username": secret.Username, "password": secret.Password}

	var granted struct {
		IDToken       string `json:"id_token"`
		ExpiresIn     int64  `json:"expires_in"`
		StatusCode    string `json:"statusCode"`
		StatusMessage string `json:"statusMessage"`
	}
	if err := b.post(ctx, b.url("/tokenized/checkout/token/grant"), headers, body, &granted); err != nil {
		return "", fmt.Errorf("commerce: bkash grant token: %w", err)
	}
	if granted.IDToken == "" {
		return "", fmt.Errorf("commerce: bkash grant token: %s %s: %w", granted.StatusCode, granted.StatusMessage, ErrCredentials)
	}

	cached.value = granted.IDToken
	cached.expires = time.Now().Add(bkashTokenTTL)
	return cached.value, nil
}

// authorise is the header set every call after the grant carries. The token is bare:
// bKash rejects a "Bearer " prefix.
func (b *Bkash) authorise(ctx context.Context, account Account) (map[string]string, error) {
	appKey, secret, err := bkashCredentials(account)
	if err != nil {
		return nil, err
	}
	token, err := b.token(ctx, appKey, secret)
	if err != nil {
		return nil, err
	}
	return map[string]string{"Authorization": token, "X-App-Key": appKey}, nil
}

// Checkout creates a payment on the school's own merchant account and returns the
// bKash-hosted page to send the learner to, and bKash's paymentID for it.
func (b *Bkash) Checkout(ctx context.Context, account Account, order Order, _ CheckoutURLs) (string, string, error) {
	if order.Price.Currency != "BDT" {
		return "", "", fmt.Errorf("%w: bkash takes BDT, not %s", ErrUnsupported, order.Price.Currency)
	}

	headers, err := b.authorise(ctx, account)
	if err != nil {
		return "", "", err
	}

	body := map[string]string{
		"mode":           "0011",
		"payerReference": order.UserID.String(),
		// bKash appends its own query string, so ours is a path segment or it is lost.
		"callbackURL":           b.callbackBase + "/" + order.TenantID.String() + "/" + order.ID.String(),
		"amount":                bkashMajor(order.Price.AmountMinor),
		"currency":              "BDT",
		"intent":                "sale",
		"merchantInvoiceNumber": order.ID.String(),
	}

	var created struct {
		StatusCode   string `json:"statusCode"`
		PaymentID    string `json:"paymentID"`
		BkashURL     string `json:"bkashURL"`
		ErrorCode    string `json:"errorCode"`
		ErrorMessage string `json:"errorMessage"`
	}
	if err := b.post(ctx, b.url("/tokenized/checkout/create"), headers, body, &created); err != nil {
		return "", "", fmt.Errorf("commerce: bkash create payment: %w", err)
	}
	if created.StatusCode != bkashOK || created.BkashURL == "" || created.PaymentID == "" {
		return "", "", fmt.Errorf("commerce: bkash create payment: %s %s: %w",
			created.ErrorCode, created.ErrorMessage, ErrGatewayUnavailable)
	}
	return created.BkashURL, created.PaymentID, nil
}

// bkashPayment is what execute and payment/status both answer with.
type bkashPayment struct {
	StatusCode            string `json:"statusCode"`
	StatusMessage         string `json:"statusMessage"`
	PaymentID             string `json:"paymentID"`
	TrxID                 string `json:"trxID"`
	TransactionStatus     string `json:"transactionStatus"`
	Amount                string `json:"amount"`
	Currency              string `json:"currency"`
	MerchantInvoiceNumber string `json:"merchantInvoiceNumber"`
	ErrorCode             string `json:"errorCode"`
	ErrorMessage          string `json:"errorMessage"`
}

/*
Confirm asks bKash what really happened, and is the only thing that settles an order.

The query is an untrusted hint: anybody can type a URL, and bKash's `signature` is
undocumented, so it is not verified and is not relied on.
*/
func (b *Bkash) Confirm(ctx context.Context, account Account, order Order, query map[string]string) (Event, error) {
	event := Event{TenantID: order.TenantID, OrderID: order.ID, ExternalID: order.ExternalID}

	paymentID := query["paymentID"]
	if paymentID == "" {
		return Event{}, fmt.Errorf("%w: bkash callback carried no paymentID", ErrNotFound)
	}
	if order.ExternalID != "" && paymentID != order.ExternalID {
		return Event{}, fmt.Errorf("%w: bkash paymentID %s is not order %s", ErrNotFound, paymentID, order.ID)
	}
	event.ExternalID = paymentID

	// bKash forbids executing a payment the learner cancelled or failed.
	if query["status"] != "success" {
		event.Kind = EventFailed
		return event, nil
	}

	headers, err := b.authorise(ctx, account)
	if err != nil {
		return Event{}, err
	}

	var paid bkashPayment
	execErr := b.post(ctx, b.url("/tokenized/checkout/execute"), headers, map[string]string{"paymentID": paymentID}, &paid)

	// Execute is one-shot — a second call answers 2062/2056 even when the money moved —
	// so a timeout or an empty answer is recovered by asking, never by executing again.
	if execErr != nil || paid.TransactionStatus == "" {
		var queried bkashPayment
		if err := b.post(ctx, b.url("/tokenized/checkout/payment/status"), headers, map[string]string{"paymentID": paymentID}, &queried); err != nil {
			return Event{}, fmt.Errorf("commerce: bkash query payment %s: %w", paymentID, err)
		}
		paid = queried
	}

	if paid.StatusCode != bkashOK || paid.TransactionStatus != bkashCompleted {
		// Initiated means the learner never finished paying: they must start over.
		event.Kind = EventFailed
		return event, nil
	}

	if err := bkashMatches(order, paid); err != nil {
		return Event{}, err
	}

	event.Kind = EventPaid
	event.PaymentExternalID = paid.TrxID
	return event, nil
}

// bkashMatches refuses a payment that is not the one this order asked for. bKash is
// told the invoice and the amount; a settlement that disagrees with either is not ours.
func bkashMatches(order Order, paid bkashPayment) error {
	if paid.MerchantInvoiceNumber != order.ID.String() {
		return fmt.Errorf("%w: bkash settled invoice %q, order is %s", ErrNotFound, paid.MerchantInvoiceNumber, order.ID)
	}

	minor, err := bkashMinor(paid.Amount)
	if err != nil {
		return fmt.Errorf("commerce: bkash amount for order %s: %w: %w", order.ID, ErrNotFound, err)
	}
	if minor != order.Price.AmountMinor {
		return fmt.Errorf("%w: bkash settled %d, order is %d", ErrNotFound, minor, order.Price.AmountMinor)
	}
	if paid.TrxID == "" {
		return fmt.Errorf("%w: bkash settled order %s with no trxID", ErrNotFound, order.ID)
	}
	return nil
}

// Refund gives the money back. It needs both ids: bKash refunds a trxID, named by the
// paymentID that produced it.
func (b *Bkash) Refund(ctx context.Context, account Account, order Order) (string, error) {
	if order.ExternalID == "" || order.PaymentExternalID == "" {
		return "", fmt.Errorf("%w: bkash refund needs a paymentID and a trxID", ErrNotFound)
	}

	headers, err := b.authorise(ctx, account)
	if err != nil {
		return "", err
	}

	// v2 lower-cases the d, alone among bKash's endpoints, and hangs off the host with
	// no version segment.
	body := map[string]string{
		"paymentId":    order.ExternalID,
		"trxId":        order.PaymentExternalID,
		"refundAmount": bkashMajor(order.Price.AmountMinor),
		"sku":          order.CourseID.String(),
		"reason":       "refund requested",
	}

	var refunded struct {
		RefundTrxID             string `json:"refundTrxId"`
		RefundTransactionStatus string `json:"refundTransactionStatus"`
		StatusCode              string `json:"statusCode"`
		ErrorCode               string `json:"errorCode"`
		ErrorMessage            string `json:"errorMessage"`
		InternalCode            string `json:"internalCode"`
		ExternalCode            string `json:"externalCode"`
		ErrorMessageEn          string `json:"errorMessageEn"`
	}
	url := b.host + "/v2/tokenized-checkout/refund/payment/transaction"
	if err := b.post(ctx, url, headers, body, &refunded); err != nil {
		return "", fmt.Errorf("commerce: bkash refund %s: %w", order.PaymentExternalID, err)
	}

	if refunded.RefundTransactionStatus != bkashCompleted || refunded.RefundTrxID == "" {
		// v2 has its own error envelope, and falls back to the v1.2 one when it answers
		// from the older stack.
		code := bkashFirst(refunded.ExternalCode, refunded.InternalCode, refunded.ErrorCode)
		message := bkashFirst(refunded.ErrorMessageEn, refunded.ErrorMessage)
		return "", fmt.Errorf("commerce: bkash refund %s: %s %s: %w",
			order.PaymentExternalID, code, message, ErrGatewayUnavailable)
	}
	return refunded.RefundTrxID, nil
}

func (b *Bkash) url(path string) string { return b.host + "/" + b.version + path }

// post is every bKash call: JSON in, JSON out, thirty seconds, and a body read under
// a limit so a wedged gateway cannot exhaust us.
func (b *Bkash) post(ctx context.Context, url string, headers map[string]string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("commerce: bkash encode request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, bkashTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("commerce: bkash build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	resp, err := b.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrGatewayUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: read response: %w", ErrGatewayUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: http %d", ErrGatewayUnavailable, resp.StatusCode)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%w: decode response: %w", ErrGatewayUnavailable, err)
	}
	return nil
}

// bkashMajor renders minor units as the decimal taka string bKash wants. Integer
// division, because a float64 loses the penny an accountant will later find.
func bkashMajor(minor int64) string {
	if minor < 0 {
		return fmt.Sprintf("-%d.%02d", -minor/100, -minor%100)
	}
	return fmt.Sprintf("%d.%02d", minor/100, minor%100)
}

// bkashMinor parses bKash's decimal taka back into minor units, without a float.
func bkashMinor(amount string) (int64, error) {
	text := strings.TrimSpace(amount)
	if text == "" {
		return 0, fmt.Errorf("commerce: bkash amount is empty")
	}

	whole, frac, _ := strings.Cut(text, ".")
	if len(frac) > 2 {
		return 0, fmt.Errorf("commerce: bkash amount %q has more than two decimals", amount)
	}
	for len(frac) < 2 {
		frac += "0"
	}
	// Digits only: ParseInt would take "+5" and "-5", and neither is an amount bKash sent.
	if !bkashDigits(whole) || !bkashDigits(frac) || len(whole) > 15 {
		return 0, fmt.Errorf("commerce: bkash amount %q is not a number", amount)
	}

	taka, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("commerce: bkash amount %q: %w", amount, err)
	}
	poisha, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("commerce: bkash amount %q: %w", amount, err)
	}
	return taka*100 + poisha, nil
}

func bkashDigits(text string) bool {
	if text == "" {
		return false
	}
	return strings.IndexFunc(text, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

// bkashFirst is the first non-empty string, for two error envelopes that disagree about
// which field carries the reason.
func bkashFirst(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
