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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/ebnsina/lms-api/internal/auth"
	"github.com/ebnsina/lms-api/internal/comms"
	"github.com/ebnsina/lms-api/internal/platform/config"
	"github.com/ebnsina/lms-api/internal/platform/database"
	"github.com/ebnsina/lms-api/internal/platform/logging"
	"github.com/ebnsina/lms-api/internal/platform/mailer"
)

// erasureInterval is how often orphaned users are swept. Daily: often enough
// that somebody who asked to be forgotten is not waiting a week, rare enough
// that a sweep never meets itself.
const erasureInterval = 24 * time.Hour

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

	// A second pool, through platform/database, because the orphan sweep is domain
	// work: it needs WithoutTenant, the statement timeout, and the slow-query log.
	// River's own pool above is for River.
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

	maintenance, err := auth.NewMaintenance(db, auth.NewPostgresRepository(), log)
	if err != nil {
		return err
	}

	orphans, err := NewEraseOrphansWorker(maintenance, log)
	if err != nil {
		return err
	}

	workers := river.NewWorkers()
	river.AddWorker(workers, emails)
	river.AddWorker(workers, orphans)

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Logger:  log,
		Workers: workers,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.WorkerMaxWorkers},
		},

		// Erasure is not urgent, and it is not optional. Daily is often enough that
		// a person who asked to be forgotten does not wait a week, and rare enough
		// that a sweep of a large platform never collides with itself. River elects
		// one leader among the workers, so N replicas still run one sweep.
		PeriodicJobs: []*river.PeriodicJob{
			river.NewPeriodicJob(
				river.PeriodicInterval(erasureInterval),
				func() (river.JobArgs, *river.InsertOpts) { return EraseOrphansArgs{}, nil },
				&river.PeriodicJobOpts{ID: EraseOrphansArgs{}.Kind()},
			),
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
