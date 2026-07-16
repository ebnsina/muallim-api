package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PostgresRepository is the Postgres-backed Repository.
type PostgresRepository struct{}

// NewPostgresRepository returns a PostgresRepository.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

func isForeignKeyViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23503"
}

func (r *PostgresRepository) CreateCategory(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewCategory) (Category, error) {
	var c Category
	err := tx.QueryRow(ctx,
		`INSERT INTO ledger_categories (tenant_id, name, kind)
		 VALUES ($1, $2, $3)
		 RETURNING id, name, kind, created_at`,
		tenantID, n.Name, n.Kind).
		Scan(&c.ID, &c.Name, &c.Kind, &c.CreatedAt)
	if err != nil {
		return Category{}, fmt.Errorf("ledger: create category: %w", err)
	}
	return c, nil
}

func (r *PostgresRepository) Categories(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit int) ([]Category, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, name, kind, created_at
		 FROM ledger_categories WHERE tenant_id = $1 ORDER BY kind, name, id LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("ledger: categories: %w", err)
	}
	return pgx.CollectRows(rows, scanCategory)
}

const entryColumns = `id, category_id, amount, currency, occurred_on, description, created_at, updated_at`

func (r *PostgresRepository) CreateEntry(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewEntry) (Entry, error) {
	rows, err := tx.Query(ctx,
		`INSERT INTO ledger_entries (tenant_id, category_id, amount, currency, occurred_on, description)
		 VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))
		 RETURNING `+entryColumns,
		tenantID, n.CategoryID, n.Amount, n.Currency, n.OccurredOn, n.Description)
	if err != nil {
		return Entry{}, fmt.Errorf("ledger: create entry: %w", err)
	}
	e, err := pgx.CollectExactlyOneRow(rows, scanEntry)
	if isForeignKeyViolation(err) {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, fmt.Errorf("ledger: create entry: %w", err)
	}
	return e, nil
}

// Two shapes, not one with an `OR $x IS NULL` on the category: a category filter
// gets the category index, the unfiltered board gets the tenant index, and neither
// leaves a Sort node on the request path. Kind narrows through the parent category.
const entriesAllSQL = `
	SELECT ` + entryColumns + ` FROM ledger_entries e
	WHERE tenant_id = $1
	  AND ($3::date IS NULL OR (occurred_on, id) < ($3, $4::uuid))
	  AND ($5::text = '' OR EXISTS (
	        SELECT 1 FROM ledger_categories c
	        WHERE c.id = e.category_id AND c.tenant_id = $1 AND c.kind = $5))
	  AND ($6::date IS NULL OR occurred_on >= $6)
	  AND ($7::date IS NULL OR occurred_on <= $7)
	ORDER BY occurred_on DESC, id DESC LIMIT $2`

const entriesByCategorySQL = `
	SELECT ` + entryColumns + ` FROM ledger_entries e
	WHERE tenant_id = $1 AND category_id = $8
	  AND ($3::date IS NULL OR (occurred_on, id) < ($3, $4::uuid))
	  AND ($5::text = '' OR EXISTS (
	        SELECT 1 FROM ledger_categories c
	        WHERE c.id = e.category_id AND c.tenant_id = $1 AND c.kind = $5))
	  AND ($6::date IS NULL OR occurred_on >= $6)
	  AND ($7::date IS NULL OR occurred_on <= $7)
	ORDER BY occurred_on DESC, id DESC LIMIT $2`

func (r *PostgresRepository) Entries(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f EntryFilter, after *cursor, limit int) ([]Entry, error) {
	var afterTime, afterID any
	if after != nil {
		afterTime, afterID = after.OccurredOn, after.ID
	}

	var rows pgx.Rows
	var err error
	if f.CategoryID != nil {
		rows, err = tx.Query(ctx, entriesByCategorySQL, tenantID, limit, afterTime, afterID, f.Kind, f.From, f.To, *f.CategoryID)
	} else {
		rows, err = tx.Query(ctx, entriesAllSQL, tenantID, limit, afterTime, afterID, f.Kind, f.From, f.To)
	}
	if err != nil {
		return nil, fmt.Errorf("ledger: entries: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanEntry)
}

// Summary sums income and expense per currency in one grouped query, then computes
// net in Go. The loop is over the grouped result set, not a per-row query.
func (r *PostgresRepository) Summary(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f EntryFilter) ([]Total, error) {
	rows, err := tx.Query(ctx,
		`SELECT c.kind, e.currency, COALESCE(SUM(e.amount), 0)
		 FROM ledger_entries e
		 JOIN ledger_categories c ON c.id = e.category_id AND c.tenant_id = e.tenant_id
		 WHERE e.tenant_id = $1
		   AND ($2::text = '' OR c.kind = $2)
		   AND ($3::uuid IS NULL OR e.category_id = $3)
		   AND ($4::date IS NULL OR e.occurred_on >= $4)
		   AND ($5::date IS NULL OR e.occurred_on <= $5)
		 GROUP BY c.kind, e.currency`,
		tenantID, f.Kind, f.CategoryID, f.From, f.To)
	if err != nil {
		return nil, fmt.Errorf("ledger: summary: %w", err)
	}
	defer rows.Close()

	byCurrency := map[string]*Total{}
	for rows.Next() {
		var kind, currency string
		var sum int64
		if err := rows.Scan(&kind, &currency, &sum); err != nil {
			return nil, fmt.Errorf("ledger: summary scan: %w", err)
		}
		t, ok := byCurrency[currency]
		if !ok {
			t = &Total{Currency: currency}
			byCurrency[currency] = t
		}
		if kind == KindIncome {
			t.Income += sum
		} else {
			t.Expense += sum
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: summary: %w", err)
	}

	totals := make([]Total, 0, len(byCurrency))
	for _, t := range byCurrency {
		t.Net = t.Income - t.Expense
		totals = append(totals, *t)
	}
	sort.Slice(totals, func(i, j int) bool { return totals[i].Currency < totals[j].Currency })
	return totals, nil
}

func scanCategory(row pgx.CollectableRow) (Category, error) {
	var c Category
	err := row.Scan(&c.ID, &c.Name, &c.Kind, &c.CreatedAt)
	return c, err
}

func scanEntry(row pgx.CollectableRow) (Entry, error) {
	var e Entry
	var desc *string
	err := row.Scan(&e.ID, &e.CategoryID, &e.Amount, &e.Currency, &e.OccurredOn, &desc, &e.CreatedAt, &e.UpdatedAt)
	if desc != nil {
		e.Description = *desc
	}
	return e, err
}
