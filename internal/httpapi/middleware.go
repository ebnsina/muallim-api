package httpapi

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// HeaderRequestID carries the correlation ID in and out of the service.
const HeaderRequestID = "X-Request-Id"

type ctxKey int

const ctxKeyRequestID ctxKey = iota

// RequestIDFrom returns the correlation ID bound to ctx, or "" when the context
// did not pass through the RequestID middleware.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}

type middleware func(http.Handler) http.Handler

// chain composes middleware so that mw[0] is outermost — it sees the request
// first and the response last.
func chain(h http.Handler, mw ...middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// requestID adopts an inbound correlation ID or mints one, binds it to the
// request context, and echoes it so a client can quote it in a support ticket.
//
// An inbound ID is trusted only as an opaque label: it is length-capped and
// never interpolated anywhere but a log field and a response header.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if id == "" || len(id) > 64 {
			id = rand.Text()
		}

		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, id)))
	})
}

// recoverPanic converts a panic into a 500 and keeps the process alive. A panic
// reaching here is a bug; it is logged with its stack and never shown to the
// client.
func recoverPanic(log *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &recorder{ResponseWriter: w}

			defer func() {
				v := recover()
				if v == nil {
					return
				}

				// http.ErrAbortHandler is the documented way to abandon a response.
				// Re-panic so the server handles it as intended.
				if v == http.ErrAbortHandler {
					panic(v)
				}

				log.ErrorContext(r.Context(), "panic recovered",
					slog.Any("panic", v),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("stack", string(debug.Stack())),
				)

				// Headers are already committed; the client will see a truncated
				// response and there is nothing further we can write.
				if rec.wrote {
					return
				}
				writeProblem(rec, r, http.StatusInternalServerError, "", log)
			}()

			next.ServeHTTP(rec, r)
		})
	}
}

// problemResponse converts any error response that is not already an RFC 9457
// document into one.
//
// This exists because the standard mux answers an unmatched path with a
// plain-text "404 page not found" and a method mismatch with a bare 405 — bodies
// no API client can parse. Registering a catch-all "/" route would intercept the
// 404 but would also mask the mux's automatic 405, since a wildcard pattern
// matches the path before the method check runs. Rewriting the response instead
// preserves the mux's routing semantics, and its Allow header with them.
//
// Responses the inner handler already rendered as problem+json — everything Huma
// raises — pass through untouched.
func problemResponse(log *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pw := &problemWriter{ResponseWriter: w, req: r, log: log}
			next.ServeHTTP(pw, r)
			pw.flush()
		})
	}
}

// problemWriter defers committing an error response until it knows whether the
// inner handler produced a problem document of its own.
type problemWriter struct {
	http.ResponseWriter
	req *http.Request
	log *slog.Logger

	code     int
	wrote    bool
	override bool
}

func (p *problemWriter) WriteHeader(code int) {
	if p.wrote {
		return
	}
	p.code = code
	p.wrote = true

	if code >= http.StatusBadRequest && !isProblem(p.Header().Get("Content-Type")) {
		// Hold the status line back; flush renders the document instead.
		p.override = true
		return
	}
	p.ResponseWriter.WriteHeader(code)
}

func (p *problemWriter) Write(b []byte) (int, error) {
	if !p.wrote {
		p.WriteHeader(http.StatusOK)
	}
	if p.override {
		// Swallow the standard library's plain-text body. Report the full length so
		// the inner handler sees a successful write.
		return len(b), nil
	}
	return p.ResponseWriter.Write(b)
}

func (p *problemWriter) flush() {
	if p.override {
		writeProblem(p.ResponseWriter, p.req, p.code, "", p.log)
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (p *problemWriter) Unwrap() http.ResponseWriter { return p.ResponseWriter }

func isProblem(contentType string) bool {
	return strings.HasPrefix(contentType, ContentTypeProblem)
}

// accessLog records one line per request. Client-side cancellation is expected
// noise and drops to debug; a deadline we blew is a signal and stays at warn.
func accessLog(log *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &recorder{ResponseWriter: w}

			next.ServeHTTP(rec, r)

			attrs := []slog.Attr{
				slog.String("request_id", RequestIDFrom(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status()),
				slog.Duration("duration", time.Since(start)),
			}

			level := slog.LevelInfo
			switch {
			case r.Context().Err() == context.Canceled:
				level = slog.LevelDebug
			case r.Context().Err() == context.DeadlineExceeded:
				level = slog.LevelWarn
			case rec.status() >= http.StatusInternalServerError:
				level = slog.LevelError
			case rec.status() >= http.StatusBadRequest:
				level = slog.LevelWarn
			}

			log.LogAttrs(r.Context(), level, "request", attrs...)
		})
	}
}

// recorder captures the status code and whether the response has been committed.
type recorder struct {
	http.ResponseWriter
	code  int
	wrote bool
}

func (r *recorder) WriteHeader(code int) {
	if r.wrote {
		return
	}
	r.code = code
	r.wrote = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}

// status reports the committed status, defaulting to 200 for a handler that
// wrote nothing — which is what net/http itself would send.
func (r *recorder) status() int {
	if r.code == 0 {
		return http.StatusOK
	}
	return r.code
}

// Unwrap lets http.ResponseController reach the underlying writer for flushing
// and hijacking, which SSE and websockets need.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
