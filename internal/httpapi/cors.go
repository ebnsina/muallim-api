package httpapi

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// preflightMaxAge caps how long a browser may cache a preflight result. Long
// enough to spare the round trip on every mutation, short enough that an origin
// revoked in config takes effect the same day.
const preflightMaxAge = 2 * time.Hour

// corsHeaders are the request headers a client may send. Authorization carries
// the access token; X-Request-Id lets a client seed the correlation ID.
var corsHeaders = []string{"Authorization", "Content-Type", HeaderRequestID}

// corsMethods are the methods a client may use.
var corsMethods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost,
	http.MethodPut, http.MethodPatch, http.MethodDelete,
}

// cors answers cross-origin requests for the origins in allowed.
//
// lms-web is served from a different origin than this API — localhost:5173 and
// localhost:8080 in development, app.example.com and api.example.com in
// production — so every browser request to us is cross-origin, and so is every
// request SvelteKit makes during server-side rendering, because its `fetch`
// deliberately enforces the same CORS rules the browser would.
//
// The allowed origin is echoed rather than answered with "*": credentials are
// coming (refresh-token cookies), and the wildcard is invalid alongside
// Access-Control-Allow-Credentials. An unlisted origin gets no CORS headers at
// all, which the browser correctly reads as a denial.
func cors(allowed []string) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			// Caches must not serve one origin's response to another.
			w.Header().Add("Vary", "Origin")

			if origin == "" || !slices.Contains(allowed, origin) {
				// Same-origin, a non-browser client, or a denied origin. Preflight from
				// a denied origin still ends here: without the headers, the browser
				// blocks the real request regardless of what we would have returned.
				if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Expose-Headers", HeaderRequestID)

			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				w.Header().Add("Vary", "Access-Control-Request-Method")
				w.Header().Add("Vary", "Access-Control-Request-Headers")
				w.Header().Set("Access-Control-Allow-Methods", strings.Join(corsMethods, ", "))
				w.Header().Set("Access-Control-Allow-Headers", strings.Join(corsHeaders, ", "))
				w.Header().Set("Access-Control-Max-Age", strconv.Itoa(int(preflightMaxAge.Seconds())))

				// A preflight is metadata, not a request for the resource. It must not
				// reach a handler.
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
