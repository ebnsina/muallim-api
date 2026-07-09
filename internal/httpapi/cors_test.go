package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const webOrigin = "http://localhost:5173"

func corsHandler() http.Handler {
	handler, _ := New(Options{
		Version:     "test",
		Logger:      discardLogger(),
		CORSOrigins: []string{webOrigin},
	})
	return handler
}

func TestCORSAllowedOrigin(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/v1/healthz", nil)
	req.Header.Set("Origin", webOrigin)

	rec := httptest.NewRecorder()
	corsHandler().ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != webOrigin {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, webOrigin)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want true", got)
	}
	// Without this the browser hides the header and the client cannot read the
	// correlation ID it is meant to quote back to us.
	if got := rec.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(got, HeaderRequestID) {
		t.Errorf("Access-Control-Expose-Headers = %q, want it to include %q", got, HeaderRequestID)
	}
	if got := rec.Header().Values("Vary"); !containsFold(got, "Origin") {
		t.Errorf("Vary = %v, want it to include Origin so caches do not cross origins", got)
	}
}

func TestCORSDeniedOrigin(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/v1/healthz", nil)
	req.Header.Set("Origin", "https://evil.example.com")

	rec := httptest.NewRecorder()
	corsHandler().ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty for an unlisted origin", got)
	}
}

// A wildcard would be silently invalid next to Allow-Credentials. The config
// layer rejects "*", so the middleware must never echo one either.
func TestCORSNeverWildcards(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/v1/healthz", nil)
	req.Header.Set("Origin", webOrigin)

	rec := httptest.NewRecorder()
	corsHandler().ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got == "*" {
		t.Error("Access-Control-Allow-Origin is *, which browsers reject alongside credentials")
	}
}

func TestCORSPreflight(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodOptions, "/v1/courses", nil)
	req.Header.Set("Origin", webOrigin)
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "authorization, content-type")

	rec := httptest.NewRecorder()
	corsHandler().ServeHTTP(rec, req)

	// 204, not 404: a preflight is metadata about a route, and must be answered
	// even for a path no handler is registered on yet.
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, http.MethodPost) {
		t.Errorf("Access-Control-Allow-Methods = %q, want it to include POST", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Authorization") {
		t.Errorf("Access-Control-Allow-Headers = %q, want it to include Authorization", got)
	}
	if got := rec.Header().Get("Access-Control-Max-Age"); got == "" {
		t.Error("Access-Control-Max-Age is unset, so the browser preflights every mutation")
	}
}

// The bug this middleware was written for. Without CORS headers on the error
// response, the browser blocks it and the client sees an opaque network failure
// instead of the 404 the API actually sent.
func TestCORSHeadersPresentOnErrorResponses(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/v1/does-not-exist", nil)
	req.Header.Set("Origin", webOrigin)

	rec := httptest.NewRecorder()
	corsHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != webOrigin {
		t.Errorf("404 response is missing Access-Control-Allow-Origin (got %q); a browser would hide it", got)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, ContentTypeProblem) {
		t.Errorf("Content-Type = %q, want %q", got, ContentTypeProblem)
	}
}

// A request with no Origin header is same-origin or a non-browser client. It must
// be served normally, without CORS headers.
func TestCORSAbsentOriginIsUntouched(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	corsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty when no Origin was sent", got)
	}
}

func containsFold(values []string, want string) bool {
	for _, v := range values {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}
