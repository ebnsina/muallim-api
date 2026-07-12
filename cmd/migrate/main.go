// Command migrate applies, rolls back, and reports on database migrations.
//
//	go run ./cmd/migrate up
//	go run ./cmd/migrate status
//	go run ./cmd/migrate down
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/ebnsina/muallim-api/internal/platform/config"
	"github.com/ebnsina/muallim-api/migrations"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	if len(args) == 0 {
		return errors.New("usage: migrate <up|down|status|reset|version>")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.DatabaseURL == "" {
		return errors.New("migrate: MUALLIM_DATABASE_URL is not set")
	}

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("migrate: open: %w", err)
	}
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("migrate: dialect: %w", err)
	}

	ctx := context.Background()

	if err := goose.RunContext(ctx, args[0], db, ".", args[1:]...); err != nil {
		return fmt.Errorf("migrate: %s: %w", args[0], err)
	}

	// River owns its own schema and its own migration ledger. Applying it on `up`
	// keeps one command sufficient to bring a database current.
	//
	// `down` deliberately does not remove it: goose rolls back one application
	// migration at a time, and tearing the job tables out from under a queue that
	// still holds work is not what anybody rolling back one migration meant.
	if args[0] == "up" {
		if err := migrateRiver(ctx, cfg.DatabaseURL); err != nil {
			return err
		}
	}
	return nil
}

func migrateRiver(ctx context.Context, databaseURL string) error {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("migrate: river pool: %w", err)
	}
	defer pool.Close()

	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("migrate: river migrator: %w", err)
	}

	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("migrate: river up: %w", err)
	}
	return nil
}
