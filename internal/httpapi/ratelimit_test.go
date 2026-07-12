package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/ebnsina/muallim-api/internal/platform/ratelimit"
)

func throttledHandler(burst int) http.Handler {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	limiter := ratelimit.New(ratelimit.Options{Burst: burst, Every: time.Minute})
	return chain(inner, requestID, throttle(limiter, discardLogger()), problemResponse(discardLogger()))
}

func post(t *testing.T, h http.Handler, path, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.RemoteAddr = remoteAddr

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestThrottleLimitsCredentialEndpoints(t *testing.T) {
	t.Parallel()

	h := throttledHandler(2)

	for i := range 2 {
		if rec := post(t, h, "/v1/auth/login", "203.0.113.7:1234"); rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i+1, rec.Code)
		}
	}

	rec := post(t, h, "/v1/auth/login", "203.0.113.7:1234")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}

	retryAfter := rec.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("a 429 must tell the client how long to wait")
	}
	if n, err := strconv.Atoi(retryAfter); err != nil || n < 1 {
		t.Errorf("Retry-After = %q, want a positive number of seconds", retryAfter)
	}

	if got := rec.Header().Get("Content-Type"); got != ContentTypeProblem {
		t.Errorf("Content-Type = %q, want a problem document", got)
	}
}

// Unlisted paths must not be throttled: the catalog is read far more often than
// anyone logs in.
func TestThrottleIgnoresUnlistedPaths(t *testing.T) {
	t.Parallel()

	h := throttledHandler(1)

	for i := range 5 {
		if rec := post(t, h, "/v1/courses", "203.0.113.7:1234"); rec.Code != http.StatusOK {
			t.Fatalf("request %d to an unthrottled path = %d, want 200", i+1, rec.Code)
		}
	}
}

// Exhausting the login budget must not lock the same client out of refreshing a
// session it already holds.
func TestThrottleKeysOnAddressAndPath(t *testing.T) {
	t.Parallel()

	h := throttledHandler(1)
	const addr = "203.0.113.7:1234"

	post(t, h, "/v1/auth/login", addr)
	if rec := post(t, h, "/v1/auth/login", addr); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("login was not limited: %d", rec.Code)
	}

	if rec := post(t, h, "/v1/auth/refresh", addr); rec.Code != http.StatusOK {
		t.Errorf("refresh = %d; exhausting the login budget locked out an unrelated endpoint", rec.Code)
	}
}

func TestThrottleKeysOnClientAddress(t *testing.T) {
	t.Parallel()

	h := throttledHandler(1)

	post(t, h, "/v1/auth/login", "203.0.113.7:1234")
	if rec := post(t, h, "/v1/auth/login", "203.0.113.7:5678"); rec.Code != http.StatusTooManyRequests {
		t.Error("a different source port was treated as a different client")
	}
	if rec := post(t, h, "/v1/auth/login", "198.51.100.4:1234"); rec.Code != http.StatusOK {
		t.Errorf("an unrelated address was limited by somebody else's traffic: %d", rec.Code)
	}
}

// A forged X-Forwarded-For must not let an attacker mint a fresh budget per
// request, nor attribute their attempts to somebody else.
func TestThrottleIgnoresForwardedForHeader(t *testing.T) {
	t.Parallel()

	h := throttledHandler(1)

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	req.RemoteAddr = "203.0.113.7:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Same peer, a different claimed address on every request.
	for i := range 3 {
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
		req.RemoteAddr = "203.0.113.7:1234"
		req.Header.Set("X-Forwarded-For", "10.0.0."+strconv.Itoa(i))

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("a forged X-Forwarded-For bought a fresh budget (request %d = %d)", i+1, rec.Code)
		}
	}
}
