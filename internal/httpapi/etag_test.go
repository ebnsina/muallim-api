package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func etagHandler(body string, status int) http.Handler {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	return chain(inner, requestID, etag(discardLogger()))
}

func TestETagIsStableForTheSameBody(t *testing.T) {
	t.Parallel()

	h := etagHandler(`{"a":1}`, http.StatusOK)

	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/v1/courses", nil))

	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/v1/courses", nil))

	tag := first.Header().Get("ETag")
	if tag == "" {
		t.Fatal("no ETag on a 200 response")
	}
	if !strings.HasPrefix(tag, `"`) || !strings.HasSuffix(tag, `"`) {
		t.Errorf("ETag %s is not a quoted opaque string", tag)
	}
	if got := second.Header().Get("ETag"); got != tag {
		t.Errorf("ETag changed between identical responses: %s then %s", tag, got)
	}
	if first.Body.String() != `{"a":1}` {
		t.Errorf("body was altered: %q", first.Body.String())
	}
}

func TestETagDiffersForDifferentBodies(t *testing.T) {
	t.Parallel()

	a := httptest.NewRecorder()
	etagHandler(`{"a":1}`, http.StatusOK).ServeHTTP(a, httptest.NewRequest(http.MethodGet, "/x", nil))

	b := httptest.NewRecorder()
	etagHandler(`{"a":2}`, http.StatusOK).ServeHTTP(b, httptest.NewRequest(http.MethodGet, "/x", nil))

	if a.Header().Get("ETag") == b.Header().Get("ETag") {
		t.Error("two different bodies produced the same ETag")
	}
}

// The payoff: a client that already holds the body gets 304 and no bytes.
func TestETagMatchReturns304WithNoBody(t *testing.T) {
	t.Parallel()

	h := etagHandler(`{"courses":[]}`, http.StatusOK)

	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/v1/courses", nil))
	tag := first.Header().Get("ETag")

	req := httptest.NewRequest(http.MethodGet, "/v1/courses", nil)
	req.Header.Set("If-None-Match", tag)

	second := httptest.NewRecorder()
	h.ServeHTTP(second, req)

	if second.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Errorf("304 carried a body of %d bytes; it must carry none", second.Body.Len())
	}
	if second.Header().Get("ETag") != tag {
		t.Error("304 must repeat the ETag so the client can keep revalidating")
	}
}

func TestETagHonoursWeakValidatorsAndWildcard(t *testing.T) {
	t.Parallel()

	h := etagHandler(`{"a":1}`, http.StatusOK)

	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/x", nil))
	tag := first.Header().Get("ETag")

	tests := map[string]string{
		"exact":            tag,
		"weak":             "W/" + tag,
		"wildcard":         "*",
		"list containing":  `"nope", ` + tag,
		"list with spaces": ` "other" ,  ` + tag + ` `,
	}

	for name, header := range tests {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set("If-None-Match", header)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotModified {
				t.Errorf("If-None-Match %q gave %d, want 304", header, rec.Code)
			}
		})
	}
}

func TestETagIgnoresNonMatchingValidator(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("If-None-Match", `"stale"`)

	rec := httptest.NewRecorder()
	etagHandler(`{"a":1}`, http.StatusOK).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for a stale validator", rec.Code)
	}
	if rec.Body.String() != `{"a":1}` {
		t.Errorf("body = %q, want the full response", rec.Body.String())
	}
}

// A tagged 404 would be a cache key for an absence, and a tagged POST response is
// meaningless. Neither is tagged.
func TestETagOnlyTagsSuccessfulReads(t *testing.T) {
	t.Parallel()

	t.Run("not on a 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		etagHandler(`{"error":true}`, http.StatusNotFound).
			ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

		if rec.Header().Get("ETag") != "" {
			t.Error("a 404 must not carry an ETag")
		}
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
		if rec.Body.String() != `{"error":true}` {
			t.Errorf("a non-200 body must pass through untouched, got %q", rec.Body.String())
		}
	})

	t.Run("not on a POST", func(t *testing.T) {
		rec := httptest.NewRecorder()
		etagHandler(`{"created":true}`, http.StatusOK).
			ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))

		if rec.Header().Get("ETag") != "" {
			t.Error("a POST response must not carry an ETag")
		}
		if rec.Body.String() != `{"created":true}` {
			t.Errorf("body = %q, want it passed through", rec.Body.String())
		}
	})
}

// A response larger than the buffer must stream through intact rather than be
// truncated or held in memory.
func TestETagStreamsOversizedBodies(t *testing.T) {
	t.Parallel()

	big := strings.Repeat("x", maxETagBody+1024)

	rec := httptest.NewRecorder()
	etagHandler(big, http.StatusOK).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != len(big) {
		t.Errorf("body length = %d, want %d — an oversized response must not be truncated",
			rec.Body.Len(), len(big))
	}
	if rec.Header().Get("ETag") != "" {
		t.Error("an oversized body must not be hashed")
	}
}
