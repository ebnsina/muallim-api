// Package logging constructs the application's structured logger.
package logging

import (
	"io"
	"log/slog"
)

// New returns a structured logger. Production emits JSON for ingestion; other
// environments emit human-readable text.
//
// Never log a password, token, or full payment detail. Redact at the call site;
// this package cannot know which fields are sensitive.
func New(w io.Writer, level slog.Level, json bool) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}

	var h slog.Handler
	if json {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h)
}
