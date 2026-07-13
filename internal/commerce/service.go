package commerce

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared here by its consumer.
type Repository interface {
	Account(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, gateway string) (Account, error)
	UpsertAccount(ctx context.Context, tx pgx.Tx, a Account) (Account, error)

	Price(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) (Money, error)
	Prices(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, courseIDs []uuid.UUID) (map[uuid.UUID]Money, error)
	SetPrice(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, m Money) error
	ClearPrice(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) error

	CourseBySlug(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string) (uuid.UUID, error)

	CreateOrder(ctx context.Context, tx pgx.Tx, o Order) (Order, error)
	AttachSession(ctx context.Context, tx pgx.Tx, tenantID, orderID uuid.UUID, externalID string) error

	// AttachPayment records the gateway's id for the money itself, learned when it
	// moved. It is what a refund is issued against.
	AttachPayment(ctx context.Context, tx pgx.Tx, tenantID, orderID uuid.UUID, paymentID string) error
	AttachRefund(ctx context.Context, tx pgx.Tx, tenantID, orderID uuid.UUID, refundID string) error
	OrderByID(ctx context.Context, tx pgx.Tx, tenantID, orderID uuid.UUID) (Order, error)
	PaidOrder(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) (Order, error)
	OrdersOf(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, limit int) ([]Order, error)
	OrdersOfTenant(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit int) ([]Order, error)

	// Settle moves an order to a terminal status, and reports whether it moved. It is
	// the whole of the webhook's idempotency: the second delivery finds the order
	// already there and changes nothing, so nobody is enrolled twice.
	Settle(ctx context.Context, tx pgx.Tx, tenantID, orderID uuid.UUID, status string) (bool, error)

	// Credentials are a workspace's own merchant secrets, stored encrypted. The
	// repository holds the ciphertext and never sees the key.
	Credentials(ctx context.Context, tx pgx.Tx, tenantID, accountID uuid.UUID) (publicID string, cipher []byte, err error)
	SetCredentials(ctx context.Context, tx pgx.Tx, tenantID, accountID uuid.UUID, publicID string, cipher []byte) error
}

/*
Sealer encrypts a workspace's gateway secret at rest.

Declared here and satisfied in platform: a store password sitting in plaintext in a
table is a store password in every backup, every replica, and every `pg_dump` that
somebody once emailed themselves. The key is not in the database.
*/
type Sealer interface {
	Seal(plaintext string) ([]byte, error)
	Open(ciphertext []byte) (string, error)
}

// AuditEntry is one line of the audit trail.
type AuditEntry struct {
	ActorID    *uuid.UUID
	Action     string
	TargetType string
	TargetID   string
	IP         netip.Addr
	UserAgent  string
	Metadata   map[string]any
}

// AuditRecorder appends to the audit trail inside the caller's transaction.
type AuditRecorder interface {
	Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e AuditEntry) error
}

/*
Enrolments turns a paid order into access.

Declared here, by the package that needs it, and satisfied in cmd/ over enroll —
which has never heard of money. It takes the caller's transaction, so the
enrolment and the paid order commit together: an order marked paid whose learner
was never enrolled is somebody who is out of pocket and locked out, and the two
halves of that must not be able to disagree.
*/
type Enrolments interface {
	GrantPurchase(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) error
	CancelPurchase(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) error
}

// Audit actions this package emits.
const (
	ActionAccountConnected = "payment_account.connected"
	ActionPriceSet         = "course.priced"
	ActionPriceCleared     = "course.price_cleared"
	ActionOrderCreated     = "order.created"
	ActionOrderPaid        = "order.paid"
	ActionOrderRefunded    = "order.refunded"
	ActionCredentialsSet   = "payment_account.credentials_set"
)

// MaxOrdersListed bounds a learner's receipts.
const MaxOrdersListed = 50

// Actor is who did something, for the audit trail.
type Actor struct {
	UserID    uuid.UUID
	IP        netip.Addr
	UserAgent string
}

// Service holds the rules and owns transaction boundaries.
type Service struct {
	db       *database.DB
	repo     Repository
	audit    AuditRecorder
	enrol    Enrolments
	seal     Sealer
	gateways map[string]Gateway
}

// NewService returns a Service. The gateways are the drivers this deployment runs.
func NewService(db *database.DB, repo Repository, audit AuditRecorder, enrol Enrolments, seal Sealer, gateways ...Gateway) *Service {
	byName := make(map[string]Gateway, len(gateways))
	for _, g := range gateways {
		byName[g.Name()] = g
	}
	return &Service{db: db, repo: repo, audit: audit, enrol: enrol, seal: seal, gateways: byName}
}

// Gateways names the drivers this deployment actually runs, so a client can offer
// the ones that exist rather than a list somebody typed into a template.
func (s *Service) Gateways() []string {
	names := make([]string, 0, len(s.gateways))
	for name := range s.gateways {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *Service) driver(name string) (Gateway, error) {
	g, ok := s.gateways[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrGatewayUnavailable, name)
	}
	return g, nil
}

// ------------------------------------------------------------------ accounts

// Connect starts a workspace's onboarding with a gateway, and remembers what it says.
func (s *Service) Connect(ctx context.Context, tenantID uuid.UUID, gateway, returnURL string, actor Actor) (string, error) {
	driver, err := s.driver(gateway)
	if err != nil {
		return "", err
	}

	// The network call is made outside the transaction. A gateway that hangs must not
	// hold a transaction open behind it.
	onboarding, err := driver.Onboard(ctx, tenantID, returnURL)
	if err != nil {
		return "", fmt.Errorf("commerce: onboard: %w", err)
	}

	err = s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		account, err := s.repo.UpsertAccount(ctx, tx, Account{
			TenantID:   tenantID,
			Gateway:    gateway,
			ExternalID: onboarding.ExternalID,
			Status:     AccountPending,
		})
		if err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &actor.UserID, Action: ActionAccountConnected,
			TargetType: "payment_account", TargetID: account.ID.String(),
			IP: actor.IP, UserAgent: actor.UserAgent,
			Metadata: map[string]any{"gateway": gateway},
		})
	})
	if err != nil {
		return "", err
	}
	return onboarding.URL, nil
}

// AccountOf returns the workspace's account, refreshed from the gateway.
//
// Refreshed, because the gateway is the authority on whether it will take money and
// our copy is a cache of an answer that changes without asking us.
func (s *Service) AccountOf(ctx context.Context, tenantID uuid.UUID, gateway string) (Account, error) {
	driver, err := s.driver(gateway)
	if err != nil {
		return Account{}, err
	}

	stored, err := s.account(ctx, tenantID, gateway)
	if err != nil {
		return Account{}, err
	}

	live, err := driver.AccountStatus(ctx, stored)
	if err != nil {
		// The gateway is down, not the account. Answer with what we last knew rather
		// than telling a school its account is broken because somebody else's is.
		return stored, nil
	}

	stored.Status, stored.ChargesEnabled = live.Status, live.ChargesEnabled
	err = s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.repo.UpsertAccount(ctx, tx, stored)
		return err
	})
	if err != nil {
		return Account{}, err
	}
	return stored, nil
}

/*
account loads the workspace's account and, if it has any, decrypts its credentials.

The secret lives in memory for the length of one gateway call and goes nowhere
else: not into an audit entry, not into a log line, and not back out of the API.
A driver that needs no credentials (Stripe, which acts on behalf of a connected
account under the platform's own key) simply never reads the field.
*/
func (s *Service) account(ctx context.Context, tenantID uuid.UUID, gateway string) (Account, error) {
	var (
		account  Account
		publicID string
		cipher   []byte
	)

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if account, err = s.repo.Account(ctx, tx, tenantID, gateway); err != nil {
			return err
		}

		publicID, cipher, err = s.repo.Credentials(ctx, tx, tenantID, account.ID)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return Account{}, err
	}

	if len(cipher) > 0 {
		secret, err := s.seal.Open(cipher)
		if err != nil {
			return Account{}, fmt.Errorf("%w: %v", ErrCredentials, err)
		}
		account.Credentials = Credentials{PublicID: publicID, Secret: secret}
	}
	return account, nil
}

/*
SetCredentials stores a workspace's own merchant credentials with a gateway.

SSLCommerz and bKash have no platform account to act on behalf of: each school is
its own merchant, and these are its keys. They are sealed before they reach the
database, and there is no endpoint that reads them back — a credential you can
retrieve is a credential that leaks the day somebody's session does.
*/
func (s *Service) SetCredentials(ctx context.Context, tenantID uuid.UUID, gateway string, creds Credentials, actor Actor) error {
	if _, err := s.driver(gateway); err != nil {
		return err
	}
	if !creds.Has() {
		return ErrCredentials
	}

	cipher, err := s.seal.Seal(creds.Secret)
	if err != nil {
		return fmt.Errorf("commerce: seal credentials: %w", err)
	}

	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		account, err := s.repo.Account(ctx, tx, tenantID, gateway)
		if errors.Is(err, ErrNotFound) {
			// The account row is the anchor the credentials hang off, so a gateway that
			// has no onboarding step still gets one. It is live the moment it has keys.
			account, err = s.repo.UpsertAccount(ctx, tx, Account{
				TenantID: tenantID, Gateway: gateway,
				ExternalID: creds.PublicID, Status: AccountActive, ChargesEnabled: true,
			})
		}
		if err != nil {
			return err
		}

		if err := s.repo.SetCredentials(ctx, tx, tenantID, account.ID, creds.PublicID, cipher); err != nil {
			return err
		}

		// The public half is audited; the secret is not. An audit trail is a thing
		// people read.
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &actor.UserID, Action: ActionCredentialsSet,
			TargetType: "payment_account", TargetID: account.ID.String(),
			IP: actor.IP, UserAgent: actor.UserAgent,
			Metadata: map[string]any{"gateway": gateway, "public_id": creds.PublicID},
		})
	})
}

// -------------------------------------------------------------------- prices

// SetPrice prices a course. A workspace that cannot be paid may not price anything.
func (s *Service) SetPrice(ctx context.Context, tenantID uuid.UUID, slug string, m Money, gateway string, actor Actor) error {
	if err := m.validate(); err != nil {
		return err
	}

	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		account, err := s.repo.Account(ctx, tx, tenantID, gateway)
		if errors.Is(err, ErrNotFound) {
			return ErrNoAccount
		}
		if err != nil {
			return err
		}
		if !account.Ready() {
			return ErrAccountNotReady
		}

		courseID, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug)
		if err != nil {
			return err
		}
		if err := s.repo.SetPrice(ctx, tx, tenantID, courseID, m); err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &actor.UserID, Action: ActionPriceSet,
			TargetType: "course", TargetID: courseID.String(),
			IP: actor.IP, UserAgent: actor.UserAgent,
			Metadata: map[string]any{"amount_minor": m.AmountMinor, "currency": m.Currency},
		})
	})
}

// ClearPrice makes a course free again. Nobody who already paid loses anything: their
// enrolment is a row, not a subscription.
func (s *Service) ClearPrice(ctx context.Context, tenantID uuid.UUID, slug string, actor Actor) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		courseID, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug)
		if err != nil {
			return err
		}
		if err := s.repo.ClearPrice(ctx, tx, tenantID, courseID); err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &actor.UserID, Action: ActionPriceCleared,
			TargetType: "course", TargetID: courseID.String(),
			IP: actor.IP, UserAgent: actor.UserAgent,
		})
	})
}

// PricesOf returns the price of each course on a page. One query, whatever the page
// holds; a course with no key is free.
func (s *Service) PricesOf(ctx context.Context, tenantID uuid.UUID, courseIDs []uuid.UUID) (map[uuid.UUID]Money, error) {
	if len(courseIDs) == 0 {
		return map[uuid.UUID]Money{}, nil
	}

	var prices map[uuid.UUID]Money
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		prices, err = s.repo.Prices(ctx, tx, tenantID, courseIDs)
		return err
	})
	if err != nil {
		return nil, err
	}
	return prices, nil
}

// PriceOf returns what a course costs, and whether it costs anything at all.
func (s *Service) PriceOf(ctx context.Context, tenantID, courseID uuid.UUID) (Money, bool, error) {
	var m Money
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		m, err = s.repo.Price(ctx, tx, tenantID, courseID)
		return err
	})
	if errors.Is(err, ErrNotFound) {
		return Money{}, false, nil
	}
	if err != nil {
		return Money{}, false, err
	}
	return m, true, nil
}

func (m Money) validate() error {
	if m.AmountMinor <= 0 {
		return fmt.Errorf("%w: an amount must be more than nothing", ErrInvalidPrice)
	}
	if len(m.Currency) != 3 {
		return fmt.Errorf("%w: a currency is three letters", ErrInvalidPrice)
	}
	return nil
}

// ------------------------------------------------------------------ checkout

/*
Buy opens a checkout for one learner and one course.

The order is written *before* the gateway is called, and its id goes into the
session's metadata. A session that succeeds against an order that does not exist is
money nobody can account for; a pending order with no session is a row we can see
and clean up.
*/
func (s *Service) Buy(ctx context.Context, tenantID uuid.UUID, slug string, learnerID uuid.UUID, gateway string, urls CheckoutURLs, actor Actor) (Checkout, error) {
	driver, err := s.driver(gateway)
	if err != nil {
		return Checkout{}, err
	}

	var (
		account Account
		order   Order
	)
	err = s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		courseID, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug)
		if err != nil {
			return err
		}

		price, err := s.repo.Price(ctx, tx, tenantID, courseID)
		if errors.Is(err, ErrNotFound) {
			return ErrFree
		}
		if err != nil {
			return err
		}

		// Already paid is not a reason to charge again. It is the one mistake a shop
		// cannot apologise its way out of.
		if _, err := s.repo.PaidOrder(ctx, tx, tenantID, courseID, learnerID); err == nil {
			return ErrAlreadyOwned
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}

		account, err = s.repo.Account(ctx, tx, tenantID, gateway)
		if errors.Is(err, ErrNotFound) {
			return ErrNoAccount
		}
		if err != nil {
			return err
		}
		if !account.Ready() {
			return ErrAccountNotReady
		}

		// The driver may be one whose merchant is the school itself, holding its own
		// keys. Read them here, in the same transaction that proved the account exists.
		publicID, cipher, err := s.repo.Credentials(ctx, tx, tenantID, account.ID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if len(cipher) > 0 {
			secret, err := s.seal.Open(cipher)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrCredentials, err)
			}
			account.Credentials = Credentials{PublicID: publicID, Secret: secret}
		}

		order, err = s.repo.CreateOrder(ctx, tx, Order{
			TenantID: tenantID, CourseID: courseID, UserID: learnerID,
			Price: price, Status: OrderPending, Gateway: gateway,
		})
		if err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &actor.UserID, Action: ActionOrderCreated,
			TargetType: "order", TargetID: order.ID.String(),
			IP: actor.IP, UserAgent: actor.UserAgent,
			Metadata: map[string]any{"course_id": courseID.String(), "amount_minor": price.AmountMinor},
		})
	})
	if err != nil {
		return Checkout{}, err
	}

	// Outside the transaction: the gateway is a network call, and a slow one must not
	// hold a row lock on the order it is about to be told about.
	url, externalID, err := driver.Checkout(ctx, account, order, urls)
	if err != nil {
		return Checkout{}, fmt.Errorf("commerce: checkout: %w", err)
	}

	err = s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.AttachSession(ctx, tx, tenantID, order.ID, externalID)
	})
	if err != nil {
		return Checkout{}, err
	}

	order.ExternalID = externalID
	return Checkout{Order: order, URL: url}, nil
}

// ------------------------------------------------------------------ webhooks

/*
Settle applies a verified webhook.

One transaction, and the whole of it is idempotent: `Settle` in the repository
moves the order only if it is still pending, and reports whether it moved. A
webhook that arrives twice — and it will, because they all do — finds the order
already paid the second time and enrols nobody twice.

The enrolment commits with the order. An order marked paid whose learner was never
enrolled is somebody out of pocket and locked out, and those two halves must not be
able to disagree.
*/
func (s *Service) Settle(ctx context.Context, gateway string, payload []byte, signature string) error {
	driver, err := s.driver(gateway)
	if err != nil {
		return err
	}

	hooked, ok := driver.(Webhooker)
	if !ok {
		return fmt.Errorf("%w: %s sends no webhook", ErrUnsupported, gateway)
	}

	event, err := hooked.Verify(payload, signature)
	if err != nil {
		return err
	}
	return s.apply(ctx, event)
}

/*
Confirm settles a gateway that has no webhook to send.

bKash has none: the learner comes back to our callback with a payment id in the
query string, and the only proof of anything is a server-to-server execute. So the
query is handed to the driver, the driver goes and *asks*, and what it comes back
with is treated exactly like a webhook would be — including the idempotency, since
a learner who reloads the callback page reloads it twice.

The order is loaded before the driver is called, because the driver needs to know
what it is confirming, and because a callback naming an order that does not exist
is the first thing an attacker tries.
*/
func (s *Service) Confirm(ctx context.Context, tenantID uuid.UUID, gateway string, orderID uuid.UUID, query map[string]string) error {
	driver, err := s.driver(gateway)
	if err != nil {
		return err
	}

	confirmer, ok := driver.(Confirmer)
	if !ok {
		return fmt.Errorf("%w: %s confirms by webhook", ErrUnsupported, gateway)
	}

	account, err := s.account(ctx, tenantID, gateway)
	if err != nil {
		return err
	}

	var order Order
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		order, err = s.repo.OrderByID(ctx, tx, tenantID, orderID)
		return err
	})
	if err != nil {
		return err
	}

	// Already settled is not a failure. It is the second click, and the answer to it
	// is the answer to the first.
	if order.Status != OrderPending {
		return nil
	}

	event, err := confirmer.Confirm(ctx, account, order, query)
	if err != nil {
		return err
	}
	event.TenantID, event.OrderID = tenantID, order.ID
	return s.apply(ctx, event)
}

/*
apply writes what a gateway told us, however it told us.

One transaction, and the whole of it is idempotent: `Settle` in the repository moves
the order only if it is still pending, and reports whether it moved. An event that
arrives twice — and they all do — finds the order already paid the second time and
enrols nobody twice.

The enrolment commits with the order. An order marked paid whose learner was never
enrolled is somebody out of pocket and locked out, and those two halves must not be
able to disagree.
*/
func (s *Service) apply(ctx context.Context, event Event) error {
	if event.Kind == EventIgnored {
		return nil
	}

	return s.db.WithTenant(ctx, event.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		order, err := s.repo.OrderByID(ctx, tx, event.TenantID, event.OrderID)
		if err != nil {
			return err
		}

		/*
			The metadata said which order; the gateway's own id says it is the same one.
			Metadata is editable in a gateway's dashboard, and an order somebody else
			pointed at is an enrolment somebody else bought.

			A refund is matched on the payment instead of the session, because that is
			what a refund is about — and a school that refunds by hand in its gateway's
			own dashboard sends us an event that never heard of a checkout session.
		*/
		switch {
		case event.Kind == EventRefunded && event.PaymentExternalID != "":
			if order.PaymentExternalID != event.PaymentExternalID {
				return fmt.Errorf("%w: the event names another payment", ErrNotFound)
			}
		case event.ExternalID != "" && order.ExternalID != event.ExternalID:
			return fmt.Errorf("%w: the event names another session", ErrNotFound)
		}

		switch event.Kind {
		case EventPaid:
			moved, err := s.repo.Settle(ctx, tx, event.TenantID, order.ID, OrderPaid)
			if err != nil || !moved {
				return err
			}

			// The id of the money itself, which is what a refund will be issued against.
			// Learned only now: no gateway knows it when the checkout is created.
			if event.PaymentExternalID != "" {
				if err := s.repo.AttachPayment(ctx, tx, event.TenantID, order.ID, event.PaymentExternalID); err != nil {
					return err
				}
			}

			if err := s.enrol.GrantPurchase(ctx, tx, order.TenantID, order.CourseID, order.UserID); err != nil {
				return err
			}
			return s.audit.Record(ctx, tx, event.TenantID, AuditEntry{
				Action: ActionOrderPaid, TargetType: "order", TargetID: order.ID.String(),
				Metadata: map[string]any{"course_id": order.CourseID.String(), "amount_minor": order.Price.AmountMinor},
			})

		case EventRefunded:
			moved, err := s.repo.Settle(ctx, tx, event.TenantID, order.ID, OrderRefunded)
			if err != nil || !moved {
				return err
			}
			if err := s.enrol.CancelPurchase(ctx, tx, order.TenantID, order.CourseID, order.UserID); err != nil {
				return err
			}
			return s.audit.Record(ctx, tx, event.TenantID, AuditEntry{
				Action: ActionOrderRefunded, TargetType: "order", TargetID: order.ID.String(),
			})

		case EventFailed:
			_, err := s.repo.Settle(ctx, tx, event.TenantID, order.ID, OrderFailed)
			return err
		}
		return nil
	})
}

// ------------------------------------------------------------------- refunds

/*
Refund gives the money back and takes the course away, together.

The gateway is called first and the database second, and that order is deliberate:
a row marked refunded against money that never moved is a learner locked out of a
course they still paid for, which is the worse of the two failures. If the write
fails after the gateway succeeded, the order is still paid and the refund is still
real — the gateway's own idempotency and this method's `WHERE status = 'paid'` make
the retry safe, and the audit trail says a refund was attempted.

The enrolment goes in the same transaction as the status. A refunded order whose
learner still has the course is a school giving its work away by accident.
*/
func (s *Service) Refund(ctx context.Context, tenantID, orderID uuid.UUID, actor Actor) (Order, error) {
	var order Order
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		order, err = s.repo.OrderByID(ctx, tx, tenantID, orderID)
		return err
	})
	if err != nil {
		return Order{}, err
	}

	if order.Status != OrderPaid {
		return Order{}, ErrNotPaid
	}

	driver, err := s.driver(order.Gateway)
	if err != nil {
		return Order{}, err
	}
	refunder, ok := driver.(Refunder)
	if !ok {
		return Order{}, fmt.Errorf("%w: %s cannot refund", ErrUnsupported, order.Gateway)
	}

	account, err := s.account(ctx, tenantID, order.Gateway)
	if err != nil {
		return Order{}, err
	}

	refundID, err := refunder.Refund(ctx, account, order)
	if err != nil {
		return Order{}, fmt.Errorf("commerce: refund: %w", err)
	}

	err = s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		moved, err := s.repo.Settle(ctx, tx, tenantID, order.ID, OrderRefunded)
		if err != nil {
			return err
		}
		if !moved {
			// Somebody refunded it while we were talking to the gateway. The gateway
			// refuses to refund twice; there is nothing left to do and nothing to undo.
			return nil
		}

		if err := s.repo.AttachRefund(ctx, tx, tenantID, order.ID, refundID); err != nil {
			return err
		}
		if err := s.enrol.CancelPurchase(ctx, tx, tenantID, order.CourseID, order.UserID); err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &actor.UserID, Action: ActionOrderRefunded,
			TargetType: "order", TargetID: order.ID.String(),
			IP: actor.IP, UserAgent: actor.UserAgent,
			Metadata: map[string]any{
				"course_id":    order.CourseID.String(),
				"amount_minor": order.Price.AmountMinor,
				"refund_id":    refundID,
			},
		})
	})
	if err != nil {
		return Order{}, err
	}

	order.Status, order.RefundExternalID = OrderRefunded, refundID
	return order, nil
}

// Orders are a workspace's own sales, newest first — what a refund is issued from.
func (s *Service) Orders(ctx context.Context, tenantID uuid.UUID, limit int) ([]Order, error) {
	if limit <= 0 || limit > MaxOrdersListed {
		limit = MaxOrdersListed
	}

	var orders []Order
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		orders, err = s.repo.OrdersOfTenant(ctx, tx, tenantID, limit)
		return err
	})
	return orders, err
}

// Receipts are a learner's own orders, newest first.
func (s *Service) Receipts(ctx context.Context, tenantID, userID uuid.UUID, limit int) ([]Order, error) {
	if limit <= 0 || limit > MaxOrdersListed {
		limit = MaxOrdersListed
	}

	var orders []Order
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		orders, err = s.repo.OrdersOf(ctx, tx, tenantID, userID, limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return orders, nil
}

// Normalise a currency the way every gateway wants it: three upper-case letters.
func normaliseCurrency(c string) string { return strings.ToUpper(strings.TrimSpace(c)) }
