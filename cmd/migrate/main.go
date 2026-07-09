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

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/ebnsina/lms-api/internal/platform/config"
	"github.com/ebnsina/lms-api/migrations"
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
		return errors.New("migrate: LMS_DATABASE_URL is not set")
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

	if err := goose.RunContext(context.Background(), args[0], db, ".", args[1:]...); err != nil {
		return fmt.Errorf("migrate: %s: %w", args[0], err)
	}
	return nil
}
