package commerce

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
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
	OrderByID(ctx context.Context, tx pgx.Tx, tenantID, orderID uuid.UUID) (Order, error)
	PaidOrder(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) (Order, error)
	OrdersOf(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, limit int) ([]Order, error)

	// Settle moves an order to a terminal status, and reports whether it moved. It is
	// the whole of the webhook's idempotency: the second delivery finds the order
	// already there and changes nothing, so nobody is enrolled twice.
	Settle(ctx context.Context, tx pgx.Tx, tenantID, orderID uuid.UUID, status string) (bool, error)
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
	gateways map[string]Gateway
}

// NewService returns a Service. The gateways are the drivers this deployment runs.
func NewService(db *database.DB, repo Repository, audit AuditRecorder, enrol Enrolments, gateways ...Gateway) *Service {
	byName := make(map[string]Gateway, len(gateways))
	for _, g := range gateways {
		byName[g.Name()] = g
	}
	return &Service{db: db, repo: repo, audit: audit, enrol: enrol, gateways: byName}
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

	var stored Account
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		stored, err = s.repo.Account(ctx, tx, tenantID, gateway)
		return err
	})
	if err != nil {
		return Account{}, err
	}

	live, err := driver.AccountStatus(ctx, stored.ExternalID)
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

	event, err := driver.Verify(payload, signature)
	if err != nil {
		return err
	}
	if event.Kind == EventIgnored {
		return nil
	}

	return s.db.WithTenant(ctx, event.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		order, err := s.repo.OrderByID(ctx, tx, event.TenantID, event.OrderID)
		if err != nil {
			return err
		}

		// The metadata said which order; the gateway's own id says it is the same one.
		// Metadata is editable in a gateway's dashboard, and an order somebody else
		// pointed at is an enrolment somebody else bought.
		if order.ExternalID != event.ExternalID {
			return fmt.Errorf("%w: the event names another session", ErrNotFound)
		}

		switch event.Kind {
		case EventPaid:
			moved, err := s.repo.Settle(ctx, tx, event.TenantID, order.ID, OrderPaid)
			if err != nil || !moved {
				return err
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
