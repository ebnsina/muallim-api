package database_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

func testDB(t *testing.T) *database.DB {
	t.Helper()

	url := os.Getenv("LMS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LMS_TEST_DATABASE_URL is not set; skipping database tests")
	}

	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	db, err := database.New(t.Context(), database.Options{URL: url, MaxConns: 4}, log)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func seedTenant(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()

	id := uuid.New()
	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, subdomain, name) VALUES ($1, $2, $3)`,
			id, "t"+id.String()[:8], "Test")
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

// The isolation guarantee, exercised through the same code path the application
// uses. Row-level security is the net beneath the application's own tenant
// filter, and a net nobody tests is a net nobody has.
func TestRowLevelSecurityIsolatesTenants(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	acme := seedTenant(t, db)
	globex := seedTenant(t, db)

	// Acme writes a course.
	err := db.WithTenant(t.Context(), acme, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO courses (tenant_id, slug, title, status) VALUES ($1, 'secret', 'Secret', 'published')`,
			acme)
		return err
	})
	if err != nil {
		t.Fatalf("acme insert: %v", err)
	}

	t.Run("acme sees its own row", func(t *testing.T) {
		var n int
		err := db.WithTenantReadOnly(t.Context(), acme, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM courses WHERE slug = 'secret'`).Scan(&n)
		})
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("acme sees %d rows, want 1", n)
		}
	})

	t.Run("globex cannot see it, even without a tenant_id filter", func(t *testing.T) {
		var n int
		err := db.WithTenantReadOnly(t.Context(), globex, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM courses WHERE slug = 'secret'`).Scan(&n)
		})
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("globex sees %d of acme's rows; row-level security is not in force", n)
		}
	})

	t.Run("a forged tenant_id is rejected by the policy's WITH CHECK", func(t *testing.T) {
		err := db.WithTenant(t.Context(), globex, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO courses (tenant_id, slug, title) VALUES ($1, 'forged', 'Forged')`,
				acme) // globex is bound; the row claims acme
			return err
		})
		if err == nil {
			t.Fatal("a tenant was able to write a row belonging to another tenant")
		}
	})
}

// Binding is transaction-local, so a connection returned to the pool cannot carry
// one tenant's setting into the next request that borrows it.
func TestTenantBindingDoesNotLeakAcrossTransactions(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	acme := seedTenant(t, db)

	if err := db.WithTenant(t.Context(), acme, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO courses (tenant_id, slug, title) VALUES ($1, 'leaky', 'Leaky')`, acme)
		return err
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// A transaction with no tenant bound must see nothing, however many pooled
	// connections have previously served a tenant.
	var n int
	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM courses`).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("an unbound transaction sees %d rows; the binding leaked across the pool", n)
	}
}

// Passing no tenant is a programming error and must fail loudly, not silently
// read another tenant's rows.
func TestWithTenantRefusesNilUUID(t *testing.T) {
	t.Parallel()

	db := testDB(t)

	err := db.WithTenant(t.Context(), uuid.Nil, func(context.Context, pgx.Tx) error {
		t.Error("the callback must not run without a tenant")
		return nil
	})
	if !errors.Is(err, database.ErrNoTenant) {
		t.Errorf("err = %v, want ErrNoTenant", err)
	}
}

// A read-only transaction must refuse a write, so a list endpoint cannot quietly
// mutate state.
func TestReadOnlyTransactionRefusesWrites(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	acme := seedTenant(t, db)

	err := db.WithTenantReadOnly(t.Context(), acme, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO courses (tenant_id, slug, title) VALUES ($1, 'nope', 'Nope')`, acme)
		return err
	})
	if err == nil {
		t.Fatal("a write succeeded inside a read-only transaction")
	}
}

func TestQueryCounterIgnoresTransactionPlumbing(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	acme := seedTenant(t, db)

	counter := &database.Counter{}
	ctx := database.WithCounter(t.Context(), counter)

	err := db.WithTenantReadOnly(ctx, acme, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		return tx.QueryRow(ctx, `SELECT count(*) FROM courses`).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}

	// One domain query. BEGIN, the tenant binding, and COMMIT are plumbing.
	if got := counter.Count(); got != 1 {
		t.Errorf("counter = %d, want 1; begin/commit/set_config must not be counted", got)
	}
}
