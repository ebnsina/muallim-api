// Package config loads and validates runtime configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Environment names. Behaviour that differs between local development and a
// deployed environment keys off these.
const (
	EnvDevelopment = "development"
	EnvStaging     = "staging"
	EnvProduction  = "production"
)

// Config is the fully validated configuration for a process. A Config returned
// by Load is always usable; there is no partially initialised state.
type Config struct {
	Env     string
	Version string

	Addr            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration

	DatabaseURL string

	// Pool sizing. Postgres costs roughly 10 MB of backend memory per connection
	// and degrades past a few hundred, so more is not better: a small pool with a
	// queue outperforms a large one that thrashes the server.
	DBMaxConns int32
	DBMinConns int32

	// DBStatementTimeout bounds any single query at the server. A query that has
	// run for this long is a bug or a missing index, and cancelling it protects
	// every other request from waiting behind it.
	DBStatementTimeout time.Duration

	// DBSlowQueryThreshold is the duration above which a query is logged at warn,
	// so an unindexed scan announces itself before a customer does.
	DBSlowQueryThreshold time.Duration

	// TenantCacheTTL bounds how long a host resolution may be stale. It also
	// bounds how long a suspended tenant keeps serving, so it is minutes, not
	// hours.
	TenantCacheTTL time.Duration

	// JWTSecret signs access tokens. A short key makes signatures forgeable
	// offline, which makes every other control irrelevant, so it is bounded here.
	JWTSecret string

	// JWTIssuer is the `iss` claim, verified on every token. It stops a token
	// minted by a sibling environment from authenticating here.
	JWTIssuer string

	// AuthRateBurst and AuthRateEvery bound credential-verifying endpoints per
	// client address per path. Each Argon2id verification allocates 64 MiB, so an
	// unlimited login endpoint is a memory-exhaustion primitive.
	AuthRateBurst int
	AuthRateEvery time.Duration

	// CORSOrigins are the exact browser origins allowed to call this API directly.
	//
	// Usually none. muallim-web is served at acme.muallim.com and reaches this API at
	// acme.muallim.com/api, which is same-origin: no preflight, no headers to grant.
	// An entry here exists for some other browser client on another origin.
	CORSOrigins []string

	// WebBaseURL is the origin of the web client. Every link this system mails —
	// verify an address, reset a password, accept an invitation — points at a page
	// there, not at this API.
	WebBaseURL string

	// SMTP delivers mail. With no host configured the process logs messages
	// instead of sending them, which is a development convenience and a disclosure
	// bug anywhere else: the body of every message we send contains a single-use
	// credential.
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string

	// MailFrom is the envelope sender, e.g. "LMS <no-reply@example.com>".
	MailFrom string

	// MailFile, when set, appends every message to a file instead of sending it,
	// so an end-to-end test can read the link a message carries. It writes
	// single-use credentials to disk in plaintext; config refuses it outside
	// development.
	MailFile string

	// StreamCustomer is the Cloudflare Stream customer code — the label in
	// `customer-<code>.cloudflarestream.com`. Empty means this deployment hosts no
	// video of its own, and the `hosted` source is refused rather than stored.
	StreamCustomer string

	// EmbedHosts are the hosts whose pages an author may put in an `iframe` with
	// the `embed` video source.
	//
	// Empty by default, and empty means the source is off. It is the one source
	// where the framed URL is an author's own, so a deployment that has not named
	// the hosts it trusts has not decided to allow it. YouTube and Vimeo are
	// unaffected: their URLs are written from a video id, not pasted.
	EmbedHosts []string

	/*
		The object store. Cloudflare R2 in production, MinIO in a container for
		development — they speak the same protocol, so there is one implementation.

		Empty means this deployment stores no files, and the assignment endpoints
		refuse rather than pretend. That is a better failure than a signed URL
		pointing at nowhere.
	*/
	S3Endpoint  string
	S3Bucket    string
	S3AccessKey string
	S3SecretKey string
	S3Region    string

	// S3PathStyle addresses a bucket as `endpoint/bucket/key`. MinIO needs it.
	S3PathStyle bool

	// WorkerMaxWorkers bounds how many jobs the worker process runs at once.
	WorkerMaxWorkers int

	LogLevel slog.Level
}

// MinJWTSecretLength is the shortest signing key we will accept. Below this, an
// attacker recovers the key offline and mints their own tokens.
const MinJWTSecretLength = 32

// IsProduction reports whether the process runs in a deployed, customer-facing
// environment. Callers use it to decide how much detail is safe to expose.
func (c Config) IsProduction() bool { return c.Env == EnvProduction }

// Load reads configuration from the environment and validates it. It returns an
// error rather than a half-built Config when a required value is missing or
// malformed, so a misconfigured process fails at startup instead of at the first
// request that happens to need the value.
func Load() (Config, error) {
	cfg := Config{
		Env:             env("LMS_ENV", EnvDevelopment),
		Version:         env("LMS_VERSION", "dev"),
		Addr:            env("LMS_ADDR", ":8080"),
		ReadTimeout:     duration("LMS_READ_TIMEOUT", 10*time.Second),
		WriteTimeout:    duration("LMS_WRITE_TIMEOUT", 30*time.Second),
		IdleTimeout:     duration("LMS_IDLE_TIMEOUT", 120*time.Second),
		ShutdownTimeout: duration("LMS_SHUTDOWN_TIMEOUT", 20*time.Second),
		DatabaseURL:     env("LMS_DATABASE_URL", ""),
		// Empty by default. muallim-web reaches this API same-origin through the edge,
		// so no browser origin needs granting; one that does is a deliberate act.
		CORSOrigins: list(env("LMS_CORS_ORIGINS", "")),

		DBMaxConns:           int32(number("LMS_DB_MAX_CONNS", 10)),
		DBMinConns:           int32(number("LMS_DB_MIN_CONNS", 2)),
		DBStatementTimeout:   duration("LMS_DB_STATEMENT_TIMEOUT", 5*time.Second),
		DBSlowQueryThreshold: duration("LMS_DB_SLOW_QUERY_THRESHOLD", 200*time.Millisecond),
		TenantCacheTTL:       duration("LMS_TENANT_CACHE_TTL", 5*time.Minute),

		JWTSecret: env("LMS_JWT_SECRET", ""),
		JWTIssuer: env("LMS_JWT_ISSUER", "muallim-api"),

		AuthRateBurst: number("LMS_AUTH_RATE_BURST", 10),
		AuthRateEvery: duration("LMS_AUTH_RATE_EVERY", 6*time.Second),

		WebBaseURL:   env("LMS_WEB_BASE_URL", "http://localhost:5173"),
		SMTPHost:     env("LMS_SMTP_HOST", ""),
		SMTPPort:     number("LMS_SMTP_PORT", 587),
		SMTPUsername: env("LMS_SMTP_USERNAME", ""),
		SMTPPassword: env("LMS_SMTP_PASSWORD", ""),
		MailFrom:     env("LMS_MAIL_FROM", "LMS <no-reply@localhost>"),
		MailFile:     env("LMS_MAIL_FILE", ""),

		StreamCustomer: env("LMS_CLOUDFLARE_STREAM_CUSTOMER", ""),
		EmbedHosts:     list(env("LMS_EMBED_ALLOWED_HOSTS", "")),

		S3Endpoint:  env("LMS_S3_ENDPOINT", ""),
		S3Bucket:    env("LMS_S3_BUCKET", ""),
		S3AccessKey: env("LMS_S3_ACCESS_KEY", ""),
		S3SecretKey: env("LMS_S3_SECRET_KEY", ""),
		S3Region:    env("LMS_S3_REGION", "auto"),
		S3PathStyle: env("LMS_S3_PATH_STYLE", "") == "true",

		WorkerMaxWorkers: number("LMS_WORKER_MAX_WORKERS", 10),
	}

	level, err := logLevel(env("LMS_LOG_LEVEL", "info"))
	if err != nil {
		return Config{}, err
	}
	cfg.LogLevel = level

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	var errs []error

	switch c.Env {
	case EnvDevelopment, EnvStaging, EnvProduction:
	default:
		errs = append(errs, fmt.Errorf("config: LMS_ENV %q must be one of %s, %s, %s",
			c.Env, EnvDevelopment, EnvStaging, EnvProduction))
	}

	if c.Addr == "" {
		errs = append(errs, errors.New("config: LMS_ADDR must not be empty"))
	}

	// A deployed environment without a database is never intentional. Locally we
	// allow it so the server can boot before any migration exists.
	if c.DatabaseURL == "" && c.Env != EnvDevelopment {
		errs = append(errs, errors.New("config: LMS_DATABASE_URL is required outside development"))
	}

	// The API sends Access-Control-Allow-Credentials, and a browser rejects that
	// alongside a wildcard origin. Failing here beats debugging it in a browser.
	if slices.Contains(c.CORSOrigins, "*") {
		errs = append(errs, errors.New(`config: LMS_CORS_ORIGINS cannot contain "*" because the API allows credentials; list exact origins`))
	}

	// An allowlist is a list of hosts, and there is no host called "*". Nothing
	// would honour a wildcard here — the check is an exact map lookup — so an
	// operator who wrote one would believe they had opened the door and be wrong in
	// the safe direction, which is still a lie worth refusing.
	/*
		A half-configured object store is worse than none: the endpoint would be set,
		the credentials would not, and every upload would fail at the signature with
		an error nobody could read.
	*/
	configured := 0
	for _, value := range []string{c.S3Endpoint, c.S3Bucket, c.S3AccessKey, c.S3SecretKey} {
		if value != "" {
			configured++
		}
	}
	if configured != 0 && configured != 4 {
		errs = append(errs, errors.New(
			"config: LMS_S3_ENDPOINT, LMS_S3_BUCKET, LMS_S3_ACCESS_KEY and LMS_S3_SECRET_KEY "+
				"must be set together, or not at all"))
	}

	if slices.Contains(c.EmbedHosts, "*") {
		errs = append(errs, errors.New(`config: LMS_EMBED_ALLOWED_HOSTS cannot contain "*"; list exact hosts, e.g. fast.wistia.net`))
	}

	// A signing secret is not optional anywhere it will actually sign anything.
	// Refusing a short one at startup beats discovering it in a forged token.
	if c.JWTSecret != "" && len(c.JWTSecret) < MinJWTSecretLength {
		errs = append(errs, fmt.Errorf("config: LMS_JWT_SECRET must be at least %d bytes, got %d",
			MinJWTSecretLength, len(c.JWTSecret)))
	}
	if c.JWTSecret == "" && c.Env != EnvDevelopment {
		errs = append(errs, errors.New("config: LMS_JWT_SECRET is required outside development"))
	}

	// Every mailed link points at the web client. A relative or malformed base URL
	// produces a link nobody can click, discovered by a user who cannot sign in.
	if u, err := url.Parse(c.WebBaseURL); err != nil || u.Scheme == "" || u.Host == "" {
		errs = append(errs, fmt.Errorf("config: LMS_WEB_BASE_URL %q must be absolute, e.g. https://app.example.com", c.WebBaseURL))
	}

	// Without SMTP the process logs messages rather than sending them, and those
	// messages carry reset tokens. That is a development affordance, and a
	// credential-disclosure bug anywhere a log is shipped somewhere.
	if c.SMTPHost == "" && c.MailFile == "" && c.Env != EnvDevelopment {
		errs = append(errs, errors.New("config: LMS_SMTP_HOST is required outside development; without it, reset tokens are written to the log"))
	}

	// The file sink writes single-use credentials to disk in plaintext. It exists
	// for end-to-end tests, and refusing it here is what keeps it there.
	if c.MailFile != "" && c.Env != EnvDevelopment {
		errs = append(errs, errors.New("config: LMS_MAIL_FILE writes credentials to disk and is refused outside development"))
	}
	if c.SMTPHost != "" && c.MailFrom == "" {
		errs = append(errs, errors.New("config: LMS_MAIL_FROM is required when LMS_SMTP_HOST is set"))
	}

	return errors.Join(errs...)
}

// StorageConfigured reports whether this deployment can accept an upload.
func (c Config) StorageConfigured() bool {
	return c.S3Endpoint != "" && c.S3Bucket != "" && c.S3AccessKey != "" && c.S3SecretKey != ""
}

// MailerConfigured reports whether mail can actually be delivered.
func (c Config) MailerConfigured() bool { return c.SMTPHost != "" }

// list splits a comma-separated environment value, trimming blanks. An empty
// value yields no entries rather than one empty string, so an unset variable
// cannot accidentally allow the origin "".
func list(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func number(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func duration(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

func logLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("config: LMS_LOG_LEVEL %q must be one of debug, info, warn, error", s)
	}
}
