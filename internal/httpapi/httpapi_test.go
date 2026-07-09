package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestRoutes(t *testing.T) {
	t.Parallel()

	handler, _ := New(Options{Version: "test", Logger: discardLogger()})

	tests := []struct {
		name        string
		method      string
		path        string
		wantStatus  int
		wantContent string
	}{
		{"liveness probe", http.MethodGet, "/v1/healthz", http.StatusOK, "application/json"},
		{"unknown route", http.MethodGet, "/does-not-exist", http.StatusNotFound, ContentTypeProblem},
		{"unknown nested route", http.MethodGet, "/v1/nowhere/nope", http.StatusNotFound, ContentTypeProblem},
		{"wrong method on known route", http.MethodDelete, "/v1/healthz", http.StatusMethodNotAllowed, ""},
		{"published contract", http.MethodGet, "/openapi.json", http.StatusOK, ""},
		{"readiness without a database", http.MethodGet, "/v1/readyz", http.StatusServiceUnavailable, ContentTypeProblem},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantContent != "" {
				if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, tt.wantContent) {
					t.Errorf("Content-Type = %q, want prefix %q", got, tt.wantContent)
				}
			}
		})
	}
}

// Every request carries a correlation ID out, whether it succeeded or not, and an
// inbound one is adopted so a trace survives across service hops.
func TestRequestIDPropagation(t *testing.T) {
	t.Parallel()

	handler, _ := New(Options{Version: "test", Logger: discardLogger()})

	t.Run("minted when absent", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/healthz", nil))

		if rec.Header().Get(HeaderRequestID) == "" {
			t.Fatal("response carries no correlation ID")
		}
	})

	t.Run("adopted when supplied", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest(http.MethodGet, "/v1/healthz", nil)
		req.Header.Set(HeaderRequestID, "upstream-trace-1")

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if got := rec.Header().Get(HeaderRequestID); got != "upstream-trace-1" {
			t.Errorf("correlation ID = %q, want the inbound value", got)
		}
	})

	t.Run("overlong inbound value is replaced", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest(http.MethodGet, "/v1/healthz", nil)
		req.Header.Set(HeaderRequestID, strings.Repeat("x", 65))

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if got := rec.Header().Get(HeaderRequestID); len(got) > 64 || got == strings.Repeat("x", 65) {
			t.Errorf("correlation ID = %q, want a freshly minted one", got)
		}
	})
}

// A 404 body must be a parseable RFC 9457 document, because that is the contract
// every generated client is written against.
func TestNotFoundIsProblemDocument(t *testing.T) {
	t.Parallel()

	handler, _ := New(Options{Version: "test", Logger: discardLogger()})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/missing", nil))

	var p problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not valid JSON: %v (body: %s)", err, rec.Body.String())
	}

	if p.Status != http.StatusNotFound {
		t.Errorf("problem.status = %d, want 404", p.Status)
	}
	if p.Instance != "/missing" {
		t.Errorf("problem.instance = %q, want %q", p.Instance, "/missing")
	}
	if p.CorrelationID == "" {
		t.Error("problem.correlation_id is empty; a user has nothing to quote in a support ticket")
	}
}

// Rewriting the mux's bare 405 into a problem document must not discard the
// Allow header it set — that header is the whole point of a 405.
func TestMethodNotAllowedPreservesAllowHeader(t *testing.T) {
	t.Parallel()

	handler, _ := New(Options{Version: "test", Logger: discardLogger()})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/healthz", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got == "" {
		t.Error("405 response dropped the Allow header")
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, ContentTypeProblem) {
		t.Errorf("Content-Type = %q, want %q", got, ContentTypeProblem)
	}
	if strings.Contains(rec.Body.String(), "Method Not Allowed\n") {
		t.Error("body is the standard library's plain text, not a problem document")
	}
}

// An error response the inner handler already rendered as problem+json — which is
// everything Huma raises — must reach the client byte for byte.
func TestExistingProblemResponsePassesThrough(t *testing.T) {
	t.Parallel()

	const body = `{"title":"Unprocessable Entity","status":422,"detail":"validation failed"}`

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ContentTypeProblem)
		w.WriteHeader(http.StatusUnprocessableEntity)
		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("write: %v", err)
		}
	})

	log := discardLogger()
	handler := chain(inner, requestID, problemResponse(log))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/courses", nil))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
	if rec.Body.String() != body {
		t.Errorf("body was rewritten\n got: %s\nwant: %s", rec.Body.String(), body)
	}
}

// A panic must become a 500 problem document, must not leak the panic value, and
// must not take the process down.
func TestPanicRecovery(t *testing.T) {
	t.Parallel()

	const secret = "connection string with password"

	mux := http.NewServeMux()
	mux.HandleFunc("/boom", func(http.ResponseWriter, *http.Request) {
		panic(secret)
	})

	log := discardLogger()
	handler := chain(mux, requestID, accessLog(log), problemResponse(log), recoverPanic(log))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, ContentTypeProblem) {
		t.Errorf("Content-Type = %q, want %q", got, ContentTypeProblem)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("500 body leaked the panic value: %s", rec.Body.String())
	}

	var p problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if p.CorrelationID == "" {
		t.Error("500 carries no correlation ID, so the log line cannot be found")
	}
}

// Once a handler has committed a status, recovery must not attempt a second
// WriteHeader — that would corrupt the response and log a spurious error.
func TestPanicAfterResponseCommitted(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/partial", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`{"partial":`)); err != nil {
			t.Errorf("write: %v", err)
		}
		panic("late failure")
	})

	log := discardLogger()
	handler := chain(mux, requestID, accessLog(log), problemResponse(log), recoverPanic(log))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/partial", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want the already-committed 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "correlation_id") {
		t.Error("recovery appended a problem document to an already-committed body")
	}
}
