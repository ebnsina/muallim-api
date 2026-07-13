package commerce

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgresRepository implements Repository against Postgres.
//
// Every method takes the caller's transaction. None takes a pool: a query outside
// db.WithTenant runs without app.tenant_id bound, and the RLS policy that reads it
// then matches nothing — silently, by returning no rows.
type PostgresRepository struct{}

// NewPostgresRepository returns one.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

const accountColumns = `id, tenant_id, gateway, external_id, status, charges_enabled, created_at, updated_at`

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.TenantID, &a.Gateway, &a.ExternalID, &a.Status,
		&a.ChargesEnabled, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// Account loads a workspace's account with one gateway.
func (r *PostgresRepository) Account(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, gateway string) (Account, error) {
	a, err := scanAccount(tx.QueryRow(ctx,
		`SELECT `+accountColumns+` FROM payment_accounts WHERE tenant_id = $1 AND gateway = $2`,
		tenantID, gateway))

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Account{}, ErrNotFound
		}
		return Account{}, fmt.Errorf("commerce: load account: %w", err)
	}
	return a, nil
}

// UpsertAccount writes it, or updates what the gateway now says about it.
//
// One statement, because connecting twice is a thing an author will do — a browser
// back button is enough — and the second attempt must not be a unique violation.
func (r *PostgresRepository) UpsertAccount(ctx context.Context, tx pgx.Tx, a Account) (Account, error) {
	stored, err := scanAccount(tx.QueryRow(ctx,
		`INSERT INTO payment_accounts (tenant_id, gateway, external_id, status, charges_enabled)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (tenant_id, gateway) DO UPDATE
		 SET external_id     = EXCLUDED.external_id,
		     status          = EXCLUDED.status,
		     charges_enabled = EXCLUDED.charges_enabled,
		     updated_at      = now()
		 RETURNING `+accountColumns,
		a.TenantID, a.Gateway, a.ExternalID, a.Status, a.ChargesEnabled))

	if err != nil {
		return Account{}, fmt.Errorf("commerce: upsert account: %w", err)
	}
	return stored, nil
}

// Price returns what a course costs. Its absence is the free course.
func (r *PostgresRepository) Price(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) (Money, error) {
	var m Money
	err := tx.QueryRow(ctx,
		`SELECT amount_minor, currency FROM course_prices WHERE tenant_id = $1 AND course_id = $2`,
		tenantID, courseID).Scan(&m.AmountMinor, &m.Currency)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Money{}, ErrNotFound
		}
		return Money{}, fmt.Errorf("commerce: load price: %w", err)
	}
	return m, nil
}

// SetPrice prices a course, or reprices it.
func (r *PostgresRepository) SetPrice(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, m Money) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO course_prices (course_id, tenant_id, amount_minor, currency)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (course_id) DO UPDATE
		 SET amount_minor = EXCLUDED.amount_minor,
		     currency     = EXCLUDED.currency,
		     updated_at   = now()`,
		courseID, tenantID, m.AmountMinor, normaliseCurrency(m.Currency))

	if err != nil {
		return fmt.Errorf("commerce: set price: %w", err)
	}
	return nil
}

// ClearPrice makes the course free again.
func (r *PostgresRepository) ClearPrice(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) error {
	_, err := tx.Exec(ctx,
		`DELETE FROM course_prices WHERE tenant_id = $1 AND course_id = $2`, tenantID, courseID)
	if err != nil {
		return fmt.Errorf("commerce: clear price: %w", err)
	}
	return nil
}

// CourseBySlug resolves the course. Published or not: an author prices a draft
// before anybody can buy it, which is the order the work actually happens in.
func (r *PostgresRepository) CourseBySlug(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT id FROM courses WHERE tenant_id = $1 AND lower(slug) = lower($2)`,
		tenantID, slug).Scan(&id)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrNotFound
		}
		return uuid.Nil, fmt.Errorf("commerce: course by slug: %w", err)
	}
	return id, nil
}

// Prices is the price of each course on a page, keyed by course. A course with no
// price simply has no key: the caller reads that as free, which is what it is.
//
// One query for the page. A price per card is the N+1 this codebase does not write.
func (r *PostgresRepository) Prices(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, courseIDs []uuid.UUID) (map[uuid.UUID]Money, error) {
	rows, err := tx.Query(ctx,
		`SELECT course_id, amount_minor, currency
		 FROM course_prices
		 WHERE tenant_id = $1 AND course_id = ANY($2)`,
		tenantID, courseIDs)
	if err != nil {
		return nil, fmt.Errorf("commerce: prices: %w", err)
	}
	defer rows.Close()

	prices := make(map[uuid.UUID]Money, len(courseIDs))
	for rows.Next() {
		var (
			id uuid.UUID
			m  Money
		)
		if err := rows.Scan(&id, &m.AmountMinor, &m.Currency); err != nil {
			return nil, fmt.Errorf("commerce: scan price: %w", err)
		}
		prices[id] = m
	}
	return prices, rows.Err()
}

const orderColumns = `id, tenant_id, course_id, user_id, amount_minor, currency, status,
	                  gateway, external_id, paid_at, refunded_at, created_at, updated_at`

func scanOrder(row pgx.Row) (Order, error) {
	var o Order
	err := row.Scan(&o.ID, &o.TenantID, &o.CourseID, &o.UserID,
		&o.Price.AmountMinor, &o.Price.Currency, &o.Status,
		&o.Gateway, &o.ExternalID, &o.PaidAt, &o.RefundedAt, &o.CreatedAt, &o.UpdatedAt)
	return o, err
}

// CreateOrder writes a pending order, before the gateway is told anything.
//
// The external id is empty until a session exists. It cannot be part of the unique
// index's promise yet, and that is fine: an order with no session is a row we can
// see and clean up, where a session with no order is money nobody can account for.
func (r *PostgresRepository) CreateOrder(ctx context.Context, tx pgx.Tx, o Order) (Order, error) {
	created, err := scanOrder(tx.QueryRow(ctx,
		`INSERT INTO orders (tenant_id, course_id, user_id, amount_minor, currency, status, gateway, external_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, '')
		 RETURNING `+orderColumns,
		o.TenantID, o.CourseID, o.UserID, o.Price.AmountMinor, normaliseCurrency(o.Price.Currency),
		OrderPending, o.Gateway))

	if err != nil {
		return Order{}, fmt.Errorf("commerce: create order: %w", err)
	}
	return created, nil
}

// AttachSession records the gateway's id for the checkout, which is the key every
// webhook about it will be matched on.
func (r *PostgresRepository) AttachSession(ctx context.Context, tx pgx.Tx, tenantID, orderID uuid.UUID, externalID string) error {
	tag, err := tx.Exec(ctx,
		`UPDATE orders SET external_id = $3, updated_at = now()
		 WHERE tenant_id = $1 AND id = $2`,
		tenantID, orderID, externalID)

	if err != nil {
		return fmt.Errorf("commerce: attach session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// OrderByID loads one order.
func (r *PostgresRepository) OrderByID(ctx context.Context, tx pgx.Tx, tenantID, orderID uuid.UUID) (Order, error) {
	o, err := scanOrder(tx.QueryRow(ctx,
		`SELECT `+orderColumns+` FROM orders WHERE tenant_id = $1 AND id = $2`, tenantID, orderID))

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Order{}, ErrNotFound
		}
		return Order{}, fmt.Errorf("commerce: load order: %w", err)
	}
	return o, nil
}

// PaidOrder is this learner's paid order for this course, if they have one. It is
// what stops a shop charging somebody twice for the same thing.
func (r *PostgresRepository) PaidOrder(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) (Order, error) {
	o, err := scanOrder(tx.QueryRow(ctx,
		`SELECT `+orderColumns+` FROM orders
		 WHERE tenant_id = $1 AND course_id = $2 AND user_id = $3 AND status = 'paid'
		 ORDER BY paid_at DESC
		 LIMIT 1`,
		tenantID, courseID, userID))

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Order{}, ErrNotFound
		}
		return Order{}, fmt.Errorf("commerce: load paid order: %w", err)
	}
	return o, nil
}

// OrdersOf is a learner's own receipts, newest first.
func (r *PostgresRepository) OrdersOf(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, limit int) ([]Order, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+orderColumns+` FROM orders
		 WHERE tenant_id = $1 AND user_id = $2
		 ORDER BY created_at DESC, id DESC
		 LIMIT $3`,
		tenantID, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("commerce: list orders: %w", err)
	}
	defer rows.Close()

	var orders []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, fmt.Errorf("commerce: scan order: %w", err)
		}
		orders = append(orders, o)
	}
	return orders, rows.Err()
}

/*
Settle moves a pending order to a terminal status, and says whether it moved.

`WHERE status = 'pending'` is the whole of the webhook's idempotency, and it is one
statement rather than a read followed by a write: two deliveries of the same event
can arrive on two connections at the same moment, and a check-then-act between them
enrols the learner twice. Here the second one updates no rows and is told so.
*/
func (r *PostgresRepository) Settle(ctx context.Context, tx pgx.Tx, tenantID, orderID uuid.UUID, status string) (bool, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE orders SET
		     status      = $3,
		     paid_at     = CASE WHEN $3 = 'paid'     THEN now() ELSE paid_at END,
		     refunded_at = CASE WHEN $3 = 'refunded' THEN now() ELSE refunded_at END,
		     updated_at  = now()
		 WHERE tenant_id = $1 AND id = $2
		   AND status = CASE WHEN $3 = 'refunded' THEN 'paid' ELSE 'pending' END`,
		tenantID, orderID, status)

	if err != nil {
		return false, fmt.Errorf("commerce: settle order: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
