// Package config loads and validates runtime configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
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

	LogLevel slog.Level
}

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

	return errors.Join(errs...)
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
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
