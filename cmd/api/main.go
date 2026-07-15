// Command api serves the Muallim HTTP API.
//
// With -dump-spec it writes the generated OpenAPI 3.1 document to stdout and
// exits without binding a port or touching the database. That document is the
// contract with muallim-web and every other client; `make spec` writes it to disk for
// their code generators.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/ebnsina/muallim-api/internal/academics"
	"github.com/ebnsina/muallim-api/internal/assess"
	"github.com/ebnsina/muallim-api/internal/assign"
	"github.com/ebnsina/muallim-api/internal/audit"
	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/catalog"
	"github.com/ebnsina/muallim-api/internal/certify"
	"github.com/ebnsina/muallim-api/internal/commerce"
	"github.com/ebnsina/muallim-api/internal/comms"
	"github.com/ebnsina/muallim-api/internal/enroll"
	"github.com/ebnsina/muallim-api/internal/exams"
	"github.com/ebnsina/muallim-api/internal/fees"
	"github.com/ebnsina/muallim-api/internal/forum"
	"github.com/ebnsina/muallim-api/internal/gamify"
	"github.com/ebnsina/muallim-api/internal/grade"
	"github.com/ebnsina/muallim-api/internal/httpapi"
	"github.com/ebnsina/muallim-api/internal/learn"
	"github.com/ebnsina/muallim-api/internal/notices"
	"github.com/ebnsina/muallim-api/internal/notify"
	"github.com/ebnsina/muallim-api/internal/platform/cache"
	"github.com/ebnsina/muallim-api/internal/platform/config"
	"github.com/ebnsina/muallim-api/internal/platform/database"
	"github.com/ebnsina/muallim-api/internal/platform/logging"
	"github.com/ebnsina/muallim-api/internal/platform/ratelimit"
	vault "github.com/ebnsina/muallim-api/internal/platform/secret"
	"github.com/ebnsina/muallim-api/internal/platform/server"
	"github.com/ebnsina/muallim-api/internal/staff"
	"github.com/ebnsina/muallim-api/internal/tenant"
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
		return errors.New("MUALLIM_DATABASE_URL is required to serve requests; use -dump-spec to emit the contract without a database")
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
	recorder := audit.NewRecorder()

	secret := cfg.JWTSecret
	if secret == "" {
		// Development only; config refuses an empty secret anywhere else. Sessions
		// do not survive a restart, which is a nuisance, not a vulnerability.
		secret = rand.Text() + rand.Text()
		log.Warn("MUALLIM_JWT_SECRET is unset; generated an ephemeral signing key. Sessions will not survive a restart.")
	}

	tokens, err := auth.NewTokenIssuer(secret, cfg.JWTIssuer)
	if err != nil {
		return err
	}

	// An insert-only River client: the driver is handed no pool, so Start would
	// refuse and Insert is disabled, but InsertTx works. This process enqueues jobs
	// inside the transactions that produce them and never works one — that is
	// cmd/worker's job, and giving the API a worker pool it must not use is how it
	// eventually uses it.
	jobs, err := river.NewClient(riverpgxv5.New(nil), &river.Config{Logger: log})
	if err != nil {
		return fmt.Errorf("create river client: %w", err)
	}

	outbox, err := comms.NewEnqueuer(jobs, cfg.WebBaseURL)
	if err != nil {
		return err
	}
	if !cfg.MailerConfigured() {
		log.Warn("MUALLIM_SMTP_HOST is unset; the worker will log messages instead of sending them, including single-use tokens")
	}

	// Session revocation crosses workspaces, so it cannot run inside the
	// tenant-bound transaction that resets a password. It is enqueued there
	// instead, on the same transaction, and worked by cmd/worker.
	revocations, err := auth.NewRiverEnqueuer(jobs, log)
	if err != nil {
		return err
	}

	authRepo := auth.NewPostgresRepository()
	identities := auth.NewService(db, authRepo, authRepo, authRepo, tokens, authAuditor{recorder}, outbox, revocations, log)
	videos, err := newVideoResolver(cfg)
	if err != nil {
		return err
	}

	// Posting a course announcement fans out a notification to every enrolled
	// learner. That is a job — enqueued in the announcement's own transaction — so
	// catalog declares an interface and this enqueuer satisfies it.
	notifyJobs, err := notify.NewRiverEnqueuer(jobs)
	if err != nil {
		return err
	}

	store, err := newObjectStore(cfg, log)
	if err != nil {
		return err
	}

	catalogRepo := catalog.NewPostgresRepository()
	courses := catalog.NewService(db, catalogRepo, catalogRepo, catalogRepo, catalogAuditor{recorder}, videos, notifyJobs, store)
	// Certificates are issued in the transaction that finishes a course. `enroll`
	// declares the interface; certify satisfies it; neither has heard of the other.
	credentials := certify.NewService(db, certify.NewPostgresRepository(), certifyAuditor{recorder})

	// Points and badges, awarded in the transaction that records a completion. Same
	// arrangement: enroll declares Rewards, gamify satisfies it, wired here.
	gamification := gamify.NewService(db, gamify.NewPostgresRepository())

	learning := enroll.NewService(db, enroll.NewPostgresRepository(), enrolAuditor{recorder},
		certificates{credentials}, gamifyRewards{gamification}).
		// Importing a cohort needs to know who holds an address. That is auth's table,
		// and this is the only place that knows both packages exist.
		WithDirectory(directory{authRepo})

	grading, err := assess.NewRiverEnqueuer(jobs)
	if err != nil {
		return err
	}
	// The gradebook. `assess` and `assign` write to it through interfaces they
	// declare; the adapters are in gradebook.go, and neither domain has heard of it.
	grades := grade.NewService(db, grade.NewPostgresRepository())

	// No mailer here: the digest sweep runs only in the worker.
	notifications := notify.NewService(db, notify.NewPostgresRepository(), nil)

	// `notes` (the learn service) notifies the author of a question when it is
	// answered. The Notifier interface is learn's; the adapter over notify is in
	// notifiers.go, so neither domain imports the other.
	notes := learn.NewService(db, learn.NewPostgresRepository(), learnNotifier{notifications})

	community := forum.NewService(db, forum.NewPostgresRepository(), forumNotifier{notifications})

	// The institution layer: academic years and terms, classes and sections. The
	// spine the school-management surface (attendance, exams, fees) will hang off.
	schooling := academics.NewService(db, academics.NewPostgresRepository(), academicsAuditor{recorder})

	// Assessment on top of the spine: grading scales, exams, marks, report cards.
	examining := exams.NewService(db, exams.NewPostgresRepository(), examsAuditor{recorder})

	// Institutional billing: fee structures, invoices, payments, student ledgers.
	billing := fees.NewService(db, fees.NewPostgresRepository(), feesAuditor{recorder})

	// The people who run the institution: teachers and the office.
	people := staff.NewService(db, staff.NewPostgresRepository(), staffAuditor{recorder})

	// Guardian notices: a school posts a message and it fans out to guardians by
	// email, in the posting transaction. `outbox` is the same enqueuer auth uses.
	noticeboard := notices.NewService(db, notices.NewPostgresRepository(), noticeBroadcaster{outbox}, noticesAuditor{recorder})

	// `learning` satisfies assess.Completions: passing a quiz completes its lesson,
	// in the transaction that recorded the grade. The interface is declared by
	// assess and satisfied by enroll, which have never heard of each other. The
	// store is where a draw_image answer's bytes live.
	quizzes := assess.NewService(db, assess.NewPostgresRepository(), assessAuditor{recorder},
		grading, learning, quizGrades{grades}, assessNotifier{notifications}, store)

	deletions, err := assign.NewRiverEnqueuer(jobs)
	if err != nil {
		return err
	}
	assignments := assign.NewService(db, assign.NewPostgresRepository(), store,
		assignAuditor{recorder}, deletions, learning, assignmentGrades{grades}, assignNotifier{notifications})

	/*
		Commerce, and the two directions it is wired in.

		A paid order enrols its buyer: `commerce` declares Enrolments, `enroll`
		satisfies it, and neither has heard of the other. And a priced course refuses
		self-enrolment: `enroll` declares Prices, `commerce` satisfies it. The adapters
		are in commerce.go; the two domains stay strangers.

		A driver is registered only when it can actually take money. Stripe needs the
		platform's key; SSLCommerz and bKash need the sealer, because their merchant is
		the school itself and its secrets are held encrypted. A driver with nothing to
		work with refuses at the door, which beats a checkout that leads nowhere.
	*/
	var gateways []commerce.Gateway
	if cfg.FakeGatewayEnabled {
		gateways = append(gateways, commerce.Fake{BaseURL: cfg.WebBaseURL, Secret: cfg.FakeGatewaySecret})
	}
	if cfg.StripeSecretKey != "" {
		gateways = append(gateways, commerce.NewStripe(
			cfg.StripeSecretKey, cfg.StripeWebhookSecret, cfg.PlatformFeeBasisPoints))
	}

	// The sealer is what makes a workspace-held secret storable at all. No key, no
	// SSLCommerz and no bKash — and it is said out loud rather than half-started.
	var sealer *vault.Sealer
	if cfg.CredentialsKey != "" {
		sealer, err = vault.NewSealer(cfg.CredentialsKey)
		if err != nil {
			return fmt.Errorf("credentials key: %w", err)
		}
	}

	if sealer != nil && cfg.SSLCommerzEnabled {
		base := commerce.SSLCommerzLive
		if cfg.SSLCommerzSandbox {
			base = commerce.SSLCommerzSandbox
		}

		driver, err := commerce.NewSSLCommerz(base, cfg.APIBaseURL+"/v1/payments/sslcommerz/ipn", nil)
		if err != nil {
			return fmt.Errorf("sslcommerz: %w", err)
		}
		gateways = append(gateways, driver)
	}

	if sealer != nil && cfg.BkashEnabled {
		gateways = append(gateways,
			commerce.NewBkash(cfg.BkashSandbox, cfg.APIBaseURL+"/v1/payments/bkash/callback", nil))
	}

	if (cfg.SSLCommerzEnabled || cfg.BkashEnabled) && sealer == nil {
		log.Warn("gateway not started: it holds a workspace's own secrets and MUALLIM_CREDENTIALS_KEY is unset",
			"sslcommerz", cfg.SSLCommerzEnabled, "bkash", cfg.BkashEnabled)
	}

	var shop *commerce.Service
	if len(gateways) > 0 {
		shop = commerce.NewService(db, commerce.NewPostgresRepository(), commerceAuditor{recorder},
			purchases{learning}, sealer, gateways...)

		// An asynchronous refund — SSLCommerz — is chased by a job worked in cmd/worker.
		// Enqueued here, in the transaction that wrote the refund.
		refundPoll, err := commerce.NewRefundPollEnqueuer(jobs)
		if err != nil {
			return err
		}
		shop = shop.WithRefundPoller(refundPoll)

		// The other direction: a course with a price is bought, not self-enrolled.
		learning = learning.WithPrices(coursePrices{commerce.NewPostgresRepository()})
	}

	handler, _ := httpapi.New(httpapi.Options{
		Version:     cfg.Version,
		Logger:      log,
		CORSOrigins: cfg.CORSOrigins,
		WebBaseURL:  cfg.WebBaseURL,
		Tenants:     tenants,
		Catalog:     courses,
		Grades:      grades,
		Certify:     credentials,
		Learn:       notes,
		Notify:      notifications,
		Forum:       community,
		Gamify:      gamification,
		Academics:   schooling,
		Exams:       examining,
		Fees:        billing,
		Staff:       people,
		Notices:     noticeboard,
		Auth:        identities,
		Enrol:       learning,
		Assess:      quizzes,
		Assign:      assignments,
		Commerce:    shop,
		DB:          db,
		AuthLimiter: ratelimit.New(ratelimit.Options{
			Burst: cfg.AuthRateBurst,
			Every: cfg.AuthRateEvery,
		}),
	})

	return server.Run(ctx, handler, server.Options{
		Addr:            cfg.Addr,
		ReadTimeout:     cfg.ReadTimeout,
		WriteTimeout:    cfg.WriteTimeout,
		IdleTimeout:     cfg.IdleTimeout,
		ShutdownTimeout: cfg.ShutdownTimeout,
	}, log)
}
