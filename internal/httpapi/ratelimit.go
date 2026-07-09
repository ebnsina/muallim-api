package httpapi

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/ebnsina/lms-api/internal/platform/ratelimit"
)

// throttledPrefixes are the paths worth limiting: everything that verifies a
// credential.
//
// Each Argon2id verification allocates 64 MiB by design. That is what makes an
// offline attack expensive, and it is also what makes an unlimited login endpoint
// a memory-exhaustion primitive for anyone with a shell and a loop.
var throttledPrefixes = []string{
	"/v1/auth/login",
	"/v1/auth/register",
	"/v1/auth/refresh",
	"/v1/auth/invitations/accept",
}

// throttle limits credential-verifying endpoints per client address per path.
//
// Keyed by address *and* path, so exhausting the login budget does not also lock
// a legitimate user out of refreshing a session they already hold.
//
// The limiter is per process. Behind N replicas a client gets N times the budget,
// which is acceptable: this stops one address from exhausting one server, it does
// not meter a paid API. A shared store on the hot path of every request is a worse
// trade until there is a reason to make it.
func throttle(limiter *ratelimit.Limiter, log *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isThrottled(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			key := clientIP(r).String() + " " + r.URL.Path

			allowed, retryAfter := limiter.Allow(key)
			if !allowed {
				seconds := max(1, int(retryAfter.Seconds()+0.999))
				w.Header().Set("Retry-After", strconv.Itoa(seconds))

				log.WarnContext(r.Context(), "rate limited",
					slog.String("path", r.URL.Path),
					slog.Int("retry_after_seconds", seconds),
				)

				writeProblem(w, r, http.StatusTooManyRequests,
					"Too many attempts. Wait before trying again.", log)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func isThrottled(path string) bool {
	for _, prefix := range throttledPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}
