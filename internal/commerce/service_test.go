package commerce_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/audit"
	"github.com/ebnsina/muallim-api/internal/commerce"
	"github.com/ebnsina/muallim-api/internal/enroll"
	"github.com/ebnsina/muallim-api/internal/platform/database"
	"github.com/ebnsina/muallim-api/internal/platform/secret"
)

func testDB(t *testing.T) *database.DB {
	t.Helper()

	url := os.Getenv("MUALLIM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MUALLIM_TEST_DATABASE_URL is not set; skipping database tests")
	}

	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	db, err := database.New(t.Context(), database.Options{URL: url, MaxConns: 4}, log)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// commerceAuditor adapts the real recorder, as cmd/ does.
type commerceAuditor struct{ recorder *audit.Recorder }

func (a commerceAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e commerce.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID: e.ActorID, Action: e.Action, TargetType: e.TargetType, TargetID: e.TargetID,
		IP: e.IP, UserAgent: e.UserAgent, Metadata: e.Metadata,
	})
}

// purchases adapts the real enrolment service, exactly as cmd/ wires it. The real
// one, because "did the buyer actually get access" is the question.
type purchases struct{ svc *enroll.Service }

func (p purchases) GrantPurchase(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) error {
	return p.svc.GrantInTx(ctx, tx, tenantID, courseID, userID, enroll.SourcePurchase)
}

func (p purchases) CancelPurchase(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) error {
	return p.svc.CancelInTx(ctx, tx, tenantID, courseID, userID)
}

type fixture struct {
	db      *database.DB
	svc     *commerce.Service
	enrol   *enroll.Service
	fake    commerce.Fake
	tenant  uuid.UUID
	course  string
	learner uuid.UUID
	actor   commerce.Actor
}

func newFixture(t *testing.T) fixture {
	t.Helper()

	db := testDB(t)
	tenantID := seedTenant(t, db)
	slug := seedCourse(t, db, tenantID)
	learner := seedUser(t, db, tenantID)

	enrolments := enroll.NewService(db, enroll.NewPostgresRepository(), enrolAuditor{audit.NewRecorder()}, nil, nil)
	fake := commerce.Fake{BaseURL: "http://localhost:5173", Secret: "test-secret"}

	sealer, err := secret.NewSealer(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}

	svc := commerce.NewService(db, commerce.NewPostgresRepository(),
		commerceAuditor{audit.NewRecorder()}, purchases{enrolments}, sealer, fake)

	return fixture{
		db: db, svc: svc, enrol: enrolments, fake: fake,
		tenant: tenantID, course: slug, learner: learner,
		actor: commerce.Actor{UserID: seedUser(t, db, tenantID)},
	}
}

// connected puts the workspace's account in place, as onboarding would.
func (f fixture) connected(t *testing.T) {
	t.Helper()

	if _, err := f.svc.Connect(t.Context(), f.tenant, commerce.GatewayFake, "http://localhost/return", f.actor); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	// The fake says yes, and AccountOf is what writes that down.
	if _, err := f.svc.AccountOf(t.Context(), f.tenant, commerce.GatewayFake); err != nil {
		t.Fatalf("AccountOf: %v", err)
	}
}

func (f fixture) priced(t *testing.T, minor int64, currency string) {
	t.Helper()

	err := f.svc.SetPrice(t.Context(), f.tenant, f.course,
		commerce.Money{AmountMinor: minor, Currency: currency}, commerce.GatewayFake, f.actor)
	if err != nil {
		t.Fatalf("SetPrice: %v", err)
	}
}

// webhook is what a gateway sends, signed the way the real ones sign.
func (f fixture) webhook(t *testing.T, kind string, order commerce.Order) error {
	t.Helper()

	payload, err := json.Marshal(commerce.FakeEvent{
		Kind:     kind,
		TenantID: order.TenantID.String(),
		OrderID:  order.ID.String(),
		Session:  order.ExternalID,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return f.svc.Settle(t.Context(), commerce.GatewayFake, payload, f.fake.Sign(payload))
}

// The whole loop: price, buy, pay, and the buyer is enrolled.
func TestBuyingACourseEnrolsTheBuyer(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.connected(t)
	f.priced(t, 120000, "bdt")

	checkout, err := f.svc.Buy(t.Context(), f.tenant, f.course, f.learner,
		commerce.GatewayFake, commerce.CheckoutURLs{Success: "/ok", Cancel: "/no"}, commerce.Actor{UserID: f.learner})
	if err != nil {
		t.Fatalf("Buy: %v", err)
	}

	if checkout.Order.Status != commerce.OrderPending {
		t.Errorf("a new order is %q, want pending", checkout.Order.Status)
	}
	if checkout.URL == "" || checkout.Order.ExternalID == "" {
		t.Fatal("a checkout with no session is money nobody can account for")
	}

	// Nothing is owned until the gateway says so. The redirect is not the truth.
	if enrolled(t, f, f.learner) {
		t.Fatal("the learner is enrolled before anybody has paid")
	}

	if err := f.webhook(t, "paid", checkout.Order); err != nil {
		t.Fatalf("webhook: %v", err)
	}

	if !enrolled(t, f, f.learner) {
		t.Error("the money arrived and the learner did not")
	}
}

/*
The one that matters: gateways deliver twice.

The second delivery must find the order already paid and do nothing — not enrol
again, not audit again. The unique index and `WHERE status = 'pending'` are what
make that true, and this is the test that says so.
*/
func TestTheSameWebhookTwiceEnrolsOnce(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.connected(t)
	f.priced(t, 50000, "bdt")

	checkout, err := f.svc.Buy(t.Context(), f.tenant, f.course, f.learner,
		commerce.GatewayFake, commerce.CheckoutURLs{}, commerce.Actor{UserID: f.learner})
	if err != nil {
		t.Fatalf("Buy: %v", err)
	}

	for i := range 3 {
		if err := f.webhook(t, "paid", checkout.Order); err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
	}

	if got := enrolmentCount(t, f, f.learner); got != 1 {
		t.Errorf("enrolments = %d after three deliveries, want 1", got)
	}
	if got := paidOrders(t, f); got != 1 {
		t.Errorf("paid orders = %d, want 1", got)
	}
}

// A refund takes the access back with the money.
func TestARefundWithdrawsTheEnrolment(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.connected(t)
	f.priced(t, 50000, "bdt")

	checkout := buy(t, f)

	if err := f.webhook(t, "paid", checkout.Order); err != nil {
		t.Fatalf("paid: %v", err)
	}
	if err := f.webhook(t, "refunded", checkout.Order); err != nil {
		t.Fatalf("refunded: %v", err)
	}

	if enrolled(t, f, f.learner) {
		t.Error("the money went back and the access did not")
	}
}

// Nobody pays twice for the same thing.
func TestBuyingWhatYouAlreadyOwnIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.connected(t)
	f.priced(t, 50000, "bdt")

	checkout := buy(t, f)
	if err := f.webhook(t, "paid", checkout.Order); err != nil {
		t.Fatalf("paid: %v", err)
	}

	_, err := f.svc.Buy(t.Context(), f.tenant, f.course, f.learner,
		commerce.GatewayFake, commerce.CheckoutURLs{}, commerce.Actor{UserID: f.learner})

	if !errors.Is(err, commerce.ErrAlreadyOwned) {
		t.Fatalf("err = %v, want ErrAlreadyOwned", err)
	}
}

// A free course is not something to check out. The learner should have been enrolled.
func TestBuyingAFreeCourseIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.connected(t)

	_, err := f.svc.Buy(t.Context(), f.tenant, f.course, f.learner,
		commerce.GatewayFake, commerce.CheckoutURLs{}, commerce.Actor{UserID: f.learner})

	if !errors.Is(err, commerce.ErrFree) {
		t.Fatalf("err = %v, want ErrFree", err)
	}
}

// A workspace that cannot be paid may not price anything. A price with nowhere to
// send the money is a checkout that ends in an apology.
func TestPricingWithoutAnAccountIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	err := f.svc.SetPrice(t.Context(), f.tenant, f.course,
		commerce.Money{AmountMinor: 1000, Currency: "bdt"}, commerce.GatewayFake, f.actor)

	if !errors.Is(err, commerce.ErrNoAccount) {
		t.Fatalf("err = %v, want ErrNoAccount", err)
	}
}

// A forged webhook is the one error in this package that is somebody attacking.
func TestAWebhookThatDoesNotVerifyIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.connected(t)
	f.priced(t, 50000, "bdt")

	checkout := buy(t, f)

	payload, _ := json.Marshal(commerce.FakeEvent{
		Kind: "paid", TenantID: f.tenant.String(),
		OrderID: checkout.Order.ID.String(), Session: checkout.Order.ExternalID,
	})

	err := f.svc.Settle(t.Context(), commerce.GatewayFake, payload, "deadbeef")

	if !errors.Is(err, commerce.ErrSignature) {
		t.Fatalf("err = %v, want ErrSignature", err)
	}
	if enrolled(t, f, f.learner) {
		t.Error("a forged webhook enrolled somebody")
	}
}

// An event that names one order and another session belongs to neither.
func TestAnEventNamingAnotherSessionIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.connected(t)
	f.priced(t, 50000, "bdt")

	checkout := buy(t, f)

	forged := checkout.Order
	forged.ExternalID = "cs_fake_somebody_elses_session"

	if err := f.webhook(t, "paid", forged); !errors.Is(err, commerce.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if enrolled(t, f, f.learner) {
		t.Error("an order somebody else pointed at bought an enrolment")
	}
}

// The price a learner paid is the price as it stood then. Raising it afterwards must
// not rewrite what is already in their receipt.
func TestTheOrderKeepsThePriceItWasSoldAt(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.connected(t)
	f.priced(t, 50000, "bdt")

	checkout := buy(t, f)
	if err := f.webhook(t, "paid", checkout.Order); err != nil {
		t.Fatalf("paid: %v", err)
	}

	f.priced(t, 999999, "bdt")

	receipts, err := f.svc.Receipts(t.Context(), f.tenant, f.learner, 10)
	if err != nil {
		t.Fatalf("Receipts: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("receipts = %d, want 1", len(receipts))
	}
	if receipts[0].Price.AmountMinor != 50000 {
		t.Errorf("the receipt now says %d; they paid 50000", receipts[0].Price.AmountMinor)
	}
}

func TestAPriceOfNothingIsNotAPrice(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.connected(t)

	for _, m := range []commerce.Money{
		{AmountMinor: 0, Currency: "bdt"},
		{AmountMinor: -100, Currency: "bdt"},
		{AmountMinor: 100, Currency: "bangladeshi taka"},
	} {
		if err := f.svc.SetPrice(t.Context(), f.tenant, f.course, m, commerce.GatewayFake, f.actor); !errors.Is(err, commerce.ErrInvalidPrice) {
			t.Errorf("%+v: err = %v, want ErrInvalidPrice", m, err)
		}
	}
}

// ------------------------------------------------------------------ helpers

// buy opens a checkout and refuses to let a failure hide: a test that ignores this
// error goes on to send a webhook about an order that was never written, and reports
// the confusion rather than the cause.
func buy(t *testing.T, f fixture) commerce.Checkout {
	t.Helper()

	checkout, err := f.svc.Buy(t.Context(), f.tenant, f.course, f.learner,
		commerce.GatewayFake, commerce.CheckoutURLs{}, commerce.Actor{UserID: f.learner})
	if err != nil {
		t.Fatalf("Buy: %v", err)
	}
	return checkout
}

func enrolled(t *testing.T, f fixture, userID uuid.UUID) bool {
	t.Helper()
	return enrolmentCount(t, f, userID) > 0
}

func enrolmentCount(t *testing.T, f fixture, userID uuid.UUID) int {
	t.Helper()

	var n int
	err := f.db.WithTenantReadOnly(t.Context(), f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM enrolments e
			 JOIN courses c ON c.id = e.course_id
			 WHERE e.tenant_id = $1 AND e.user_id = $2 AND lower(c.slug) = lower($3)
			   AND e.status = 'active'`,
			f.tenant, userID, f.course).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count enrolments: %v", err)
	}
	return n
}

func paidOrders(t *testing.T, f fixture) int {
	t.Helper()

	var n int
	err := f.db.WithTenantReadOnly(t.Context(), f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM orders WHERE tenant_id = $1 AND status = 'paid'`, f.tenant).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count orders: %v", err)
	}
	return n
}

func seedTenant(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()

	id := uuid.New()
	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, subdomain, name) VALUES ($1, $2, 'Test')`,
			id, "c"+id.String()[:8])
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_ = db.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, id)
			return err
		})
	})
	return id
}

func seedUser(t *testing.T, db *database.DB, tenantID uuid.UUID) uuid.UUID {
	t.Helper()

	id := uuid.New()
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO users (id, email, password_hash, name) VALUES ($1, $2, 'x', 'U')`,
			id, "u-"+id.String()[:8]+"@example.test"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'student')`, tenantID, id)
		return err
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func seedCourse(t *testing.T, db *database.DB, tenantID uuid.UUID) string {
	t.Helper()

	slug := fmt.Sprintf("c-%s", uuid.NewString()[:8])
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO courses (tenant_id, slug, title, status) VALUES ($1, $2, 'Course', 'published')`,
			tenantID, slug)
		return err
	})
	if err != nil {
		t.Fatalf("seed course: %v", err)
	}
	return slug
}

// enrolAuditor adapts the recorder for enroll, as cmd/ does.
type enrolAuditor struct{ recorder *audit.Recorder }

func (a enrolAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e enroll.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID: e.ActorID, Action: e.Action, TargetType: e.TargetType, TargetID: e.TargetID,
		IP: e.IP, UserAgent: e.UserAgent, Metadata: e.Metadata,
	})
}

/*
The refund a workspace issues, rather than one a gateway announced.

The money goes back and the course goes away, in one transaction. Before this
existed the learner could cancel a purchased enrolment themselves: the enrolment
went, the paid order stayed paid, and re-enrolling asked them to buy what they had
already bought.
*/
func TestRefundingAnOrderWithdrawsTheEnrolment(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.connected(t)
	f.priced(t, 120000, "bdt")

	checkout, err := f.svc.Buy(t.Context(), f.tenant, f.course, f.learner,
		commerce.GatewayFake, commerce.CheckoutURLs{Success: "/ok", Cancel: "/no"}, commerce.Actor{UserID: f.learner})
	if err != nil {
		t.Fatalf("Buy: %v", err)
	}
	if err := f.webhook(t, "paid", checkout.Order); err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if !enrolled(t, f, f.learner) {
		t.Fatal("the learner paid and was not enrolled")
	}

	order, err := f.svc.Refund(t.Context(), f.tenant, checkout.Order.ID, f.actor)
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}

	if order.Status != commerce.OrderRefunded {
		t.Errorf("the order is %q, want refunded", order.Status)
	}
	if order.RefundExternalID == "" {
		t.Error("a refund with no id from the gateway is a refund nobody can trace")
	}
	if enrolled(t, f, f.learner) {
		t.Error("the money went back and the learner kept the course")
	}
}

// A second refund finds nothing to refund. The gateway would refuse it anyway, and
// the order is the only place that can say so first.
func TestRefundingTwiceIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.connected(t)
	f.priced(t, 120000, "bdt")

	checkout, err := f.svc.Buy(t.Context(), f.tenant, f.course, f.learner,
		commerce.GatewayFake, commerce.CheckoutURLs{Success: "/ok", Cancel: "/no"}, commerce.Actor{UserID: f.learner})
	if err != nil {
		t.Fatalf("Buy: %v", err)
	}
	if err := f.webhook(t, "paid", checkout.Order); err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if _, err := f.svc.Refund(t.Context(), f.tenant, checkout.Order.ID, f.actor); err != nil {
		t.Fatalf("Refund: %v", err)
	}

	if _, err := f.svc.Refund(t.Context(), f.tenant, checkout.Order.ID, f.actor); !errors.Is(err, commerce.ErrNotPaid) {
		t.Fatalf("a second refund returned %v, want ErrNotPaid", err)
	}
}

// An order nobody paid has no money in it to give back.
func TestRefundingAnUnpaidOrderIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.connected(t)
	f.priced(t, 120000, "bdt")

	checkout, err := f.svc.Buy(t.Context(), f.tenant, f.course, f.learner,
		commerce.GatewayFake, commerce.CheckoutURLs{Success: "/ok", Cancel: "/no"}, commerce.Actor{UserID: f.learner})
	if err != nil {
		t.Fatalf("Buy: %v", err)
	}

	if _, err := f.svc.Refund(t.Context(), f.tenant, checkout.Order.ID, f.actor); !errors.Is(err, commerce.ErrNotPaid) {
		t.Fatalf("refunding a pending order returned %v, want ErrNotPaid", err)
	}
}

// The seal is not decoration: what the driver is handed must be what the workspace
// typed, and what the table holds must not be.
func TestCredentialsComeBackToTheDriverAndNotToAnybodyElse(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	err := f.svc.SetCredentials(t.Context(), f.tenant, commerce.GatewayFake,
		commerce.Credentials{PublicID: "store-42", Secret: "hunter2"}, f.actor)
	if err != nil {
		t.Fatalf("SetCredentials: %v", err)
	}

	var cipher []byte
	err = f.db.WithTenantReadOnly(t.Context(), f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT secret_cipher FROM payment_credentials WHERE tenant_id = $1`,
			f.tenant).Scan(&cipher)
	})
	if err != nil {
		t.Fatalf("read the ciphertext: %v", err)
	}

	if strings.Contains(string(cipher), "hunter2") {
		t.Fatal("the store password is readable in the table")
	}
}
