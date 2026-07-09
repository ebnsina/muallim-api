// Package server runs an HTTP server with a bounded, graceful shutdown.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Options configures Run. It deliberately does not depend on the config package:
// platform packages stay independent of one another, and the caller in cmd/ does
// the wiring.
type Options struct {
	Addr            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
}

// Run starts an HTTP server and blocks until ctx is cancelled, then drains
// in-flight requests within ShutdownTimeout.
//
// A cancelled context is the normal exit path, not an error: Run returns nil
// when shutdown completes cleanly, and an error only when the listener failed or
// requests were still in flight when the drain deadline expired.
func Run(ctx context.Context, h http.Handler, opts Options, log *slog.Logger) error {
	srv := &http.Server{
		Addr:              opts.Addr,
		Handler:           h,
		ReadTimeout:       opts.ReadTimeout,
		ReadHeaderTimeout: opts.ReadTimeout,
		WriteTimeout:      opts.WriteTimeout,
		IdleTimeout:       opts.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
	}

	// Buffered so the goroutine can exit even if nobody is left to receive.
	listenErr := make(chan error, 1)
	go func() {
		log.Info("http server listening", slog.String("addr", opts.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenErr <- fmt.Errorf("server: listen on %s: %w", opts.Addr, err)
			return
		}
		listenErr <- nil
	}()

	select {
	case err := <-listenErr:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received, draining", slog.Duration("timeout", opts.ShutdownTimeout))
	}

	// A fresh context: ctx is already cancelled, and Shutdown needs a live
	// deadline to bound the drain.
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opts.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(drainCtx); err != nil {
		// Requests were still in flight at the deadline. Close forces them shut so
		// the process can exit rather than hang.
		if closeErr := srv.Close(); closeErr != nil {
			return errors.Join(
				fmt.Errorf("server: graceful shutdown: %w", err),
				fmt.Errorf("server: force close: %w", closeErr),
			)
		}
		return fmt.Errorf("server: graceful shutdown: %w", err)
	}

	log.Info("http server stopped cleanly")
	return nil
}
