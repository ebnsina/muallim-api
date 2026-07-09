// Command worker runs background jobs.
//
// Grading, transcoding, email, transcription, and analytics rollups belong here
// rather than in a request handler: anything that can exceed ~200ms or calls a
// third party is a job. Jobs are retried, so jobs are idempotent.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/ebnsina/lms-api/internal/comms"
	"github.com/ebnsina/lms-api/internal/platform/config"
	"github.com/ebnsina/lms-api/internal/platform/logging"
	"github.com/ebnsina/lms-api/internal/platform/mailer"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := logging.New(os.Stdout, cfg.LogLevel, cfg.IsProduction())

	if cfg.DatabaseURL == "" {
		return errors.New("LMS_DATABASE_URL is required")
	}

	// Cancelled on SIGINT/SIGTERM, which begins the graceful drain: River stops
	// fetching new jobs and waits for the running ones.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A plain pool rather than platform/database: that package binds tenants to
	// transactions and caps every statement at five seconds, neither of which
	// suits River's own queries. A job that needs tenant-scoped data opens its own
	// database.DB and goes through WithTenant like everything else.
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("worker: create pool: %w", err)
	}
	defer pool.Close()

	sender, err := newSender(cfg, log)
	if err != nil {
		return err
	}

	emails, err := comms.NewEmailWorker(sender, log)
	if err != nil {
		return err
	}

	workers := river.NewWorkers()
	river.AddWorker(workers, emails)

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Logger:  log,
		Workers: workers,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.WorkerMaxWorkers},
		},
	})
	if err != nil {
		return fmt.Errorf("worker: create river client: %w", err)
	}

	log.Info("starting worker", "env", cfg.Env, "version", cfg.Version, "max_workers", cfg.WorkerMaxWorkers)

	if err := client.Start(ctx); err != nil {
		return fmt.Errorf("worker: start: %w", err)
	}

	<-ctx.Done()
	log.Info("draining")

	// Stop waits for running jobs to finish. The context is already cancelled, so
	// use a fresh one or the drain is cancelled before it begins.
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer cancel()

	if err := client.Stop(stopCtx); err != nil {
		return fmt.Errorf("worker: stop: %w", err)
	}

	log.Info("stopped")
	return nil
}

// newSender picks a mail driver. Config has already refused both the file sink
// and an unset SMTP host outside development, so neither branch below is
// reachable from a deployed environment.
func newSender(cfg config.Config, log *slog.Logger) (comms.Sender, error) {
	if cfg.MailFile != "" {
		log.Warn("LMS_MAIL_FILE is set; email will be written to disk in plaintext, not sent",
			slog.String("path", cfg.MailFile))
		return mailer.NewFile(cfg.MailFile)
	}

	if !cfg.MailerConfigured() {
		log.Warn("no SMTP host configured; email will be logged, not sent")
		return mailer.NewLog(log), nil
	}

	return mailer.NewSMTP(mailer.SMTPOptions{
		Host:     cfg.SMTPHost,
		Port:     cfg.SMTPPort,
		Username: cfg.SMTPUsername,
		Password: cfg.SMTPPassword,
		From:     cfg.MailFrom,
	})
}
