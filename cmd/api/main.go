// Command api serves the LMS HTTP API.
//
// With -dump-spec it writes the generated OpenAPI 3.1 document to stdout and
// exits without binding a port or touching the database. That document is the
// contract with lms-web and every other client; `make spec` writes it to disk for
// their code generators.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/ebnsina/lms-api/internal/catalog"
	"github.com/ebnsina/lms-api/internal/httpapi"
	"github.com/ebnsina/lms-api/internal/platform/cache"
	"github.com/ebnsina/lms-api/internal/platform/config"
	"github.com/ebnsina/lms-api/internal/platform/database"
	"github.com/ebnsina/lms-api/internal/platform/logging"
	"github.com/ebnsina/lms-api/internal/platform/server"
	"github.com/ebnsina/lms-api/internal/tenant"
)

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet if config failed, so report to stderr.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	dumpSpec := flag.Bool("dump-spec", false, "write the OpenAPI 3.1 spec to stdout and exit")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// When dumping the spec, stdout carries the document and nothing else.
	logOut := io.Writer(os.Stdout)
	if *dumpSpec {
		logOut = io.Discard
	}
	log := logging.New(logOut, cfg.LogLevel, cfg.IsProduction())

	if *dumpSpec {
		// No services and no database: the routes are registered so they appear in
		// the document, and no handler runs.
		_, api := httpapi.New(httpapi.Options{Version: cfg.Version, Logger: log})

		spec, err := api.OpenAPI().MarshalJSON()
		if err != nil {
			return fmt.Errorf("marshal openapi spec: %w", err)
		}
		if _, err := os.Stdout.Write(spec); err != nil {
			return fmt.Errorf("write openapi spec: %w", err)
		}
		return nil
	}

	// Cancelled on SIGINT/SIGTERM, which is what begins the graceful drain.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting", "env", cfg.Env, "version", cfg.Version)

	if cfg.DatabaseURL == "" {
		return errors.New("LMS_DATABASE_URL is required to serve requests; use -dump-spec to emit the contract without a database")
	}

	db, err := database.New(ctx, database.Options{
		URL:                cfg.DatabaseURL,
		MaxConns:           cfg.DBMaxConns,
		MinConns:           cfg.DBMinConns,
		StatementTimeout:   cfg.DBStatementTimeout,
		SlowQueryThreshold: cfg.DBSlowQueryThreshold,
	}, log)
	if err != nil {
		return err
	}
	defer db.Close()

	// Wiring lives here and nowhere else. Domain packages receive their
	// dependencies; they never construct them, and they never reach for a global.
	tenants := tenant.NewService(
		tenant.NewPostgresRepository(db),
		cache.New[tenant.Tenant](cache.Options{
			TTL:         cfg.TenantCacheTTL,
			NegativeTTL: cfg.TenantCacheTTL / 4,
			MaxEntries:  4096,
		}),
	)
	courses := catalog.NewService(db, catalog.NewPostgresRepository())

	handler, _ := httpapi.New(httpapi.Options{
		Version:     cfg.Version,
		Logger:      log,
		CORSOrigins: cfg.CORSOrigins,
		Tenants:     tenants,
		Catalog:     courses,
		DB:          db,
	})

	return server.Run(ctx, handler, server.Options{
		Addr:            cfg.Addr,
		ReadTimeout:     cfg.ReadTimeout,
		WriteTimeout:    cfg.WriteTimeout,
		IdleTimeout:     cfg.IdleTimeout,
		ShutdownTimeout: cfg.ShutdownTimeout,
	}, log)
}
