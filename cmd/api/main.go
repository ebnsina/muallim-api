// Command api serves the LMS HTTP API.
//
// With -dump-spec it writes the generated OpenAPI 3.1 document to stdout and
// exits without binding a port. That document is the contract with lms-web and
// every other client; `make spec` writes it to disk for their code generators.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/ebnsina/lms-api/internal/httpapi"
	"github.com/ebnsina/lms-api/internal/platform/config"
	"github.com/ebnsina/lms-api/internal/platform/logging"
	"github.com/ebnsina/lms-api/internal/platform/server"
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

	handler, api := httpapi.New(httpapi.Options{
		Version: cfg.Version,
		Logger:  log,
	})

	if *dumpSpec {
		spec, err := api.OpenAPI().MarshalJSON()
		if err != nil {
			return fmt.Errorf("marshal openapi spec: %w", err)
		}
		if _, err := os.Stdout.Write(spec); err != nil {
			return fmt.Errorf("write openapi spec: %w", err)
		}
		return nil
	}

	log.Info("starting",
		"env", cfg.Env,
		"version", cfg.Version,
	)

	// Cancelled on SIGINT/SIGTERM, which is what begins the graceful drain.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return server.Run(ctx, handler, server.Options{
		Addr:            cfg.Addr,
		ReadTimeout:     cfg.ReadTimeout,
		WriteTimeout:    cfg.WriteTimeout,
		IdleTimeout:     cfg.IdleTimeout,
		ShutdownTimeout: cfg.ShutdownTimeout,
	}, log)
}
