package httpapi

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/lms-api/internal/auth"
	"github.com/ebnsina/lms-api/internal/tenant"
)

type principalKey struct{}

// authenticate verifies a bearer token and binds the principal to the request
// context. It does not require one: an absent or malformed Authorization header
// simply leaves the request anonymous.
//
// Rejecting the anonymous case is the job of `requirePrincipal`, called by the
// operations that need it. Middleware establishes *who you are*; the handler and
// the service decide *what you may do*. A middleware that also authorises means
// every new route is protected only if someone remembers to list it.
func authenticate(svc *auth.Service, log *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				next.ServeHTTP(w, r)
				return
			}

			principal, err := svc.Verify(raw)
			if err != nil {
				// An invalid token is not an anonymous request. Saying so here beats a
				// confusing 403 from a handler that thinks nobody is logged in.
				writeProblem(w, r, http.StatusUnauthorized, "The access token is invalid or has expired.", log)
				return
			}

			// The token names a tenant. If it does not name *this* tenant, it was
			// minted elsewhere: a valid token for workspace A must not authenticate
			// its bearer on workspace B.
			if t, ok := tenant.FromContext(r.Context()); ok && t.ID != principal.TenantID {
				writeProblem(w, r, http.StatusUnauthorized, "The access token was issued for a different workspace.", log)
				return
			}

			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal)))
		})
	}
}

// bearerToken extracts the credential from an Authorization header. The scheme
// is compared case-insensitively, as RFC 9110 requires.
func bearerToken(header string) (string, bool) {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// principalFrom returns the authenticated caller bound to ctx.
func principalFrom(ctx context.Context) (auth.Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(auth.Principal)
	return p, ok
}

// requirePrincipal returns the caller, or a 401 for an operation that needs one.
func requirePrincipal(ctx context.Context) (auth.Principal, error) {
	p, ok := principalFrom(ctx)
	if !ok {
		return auth.Principal{}, huma.Error401Unauthorized("Authentication is required.")
	}
	return p, nil
}

// requirePermission returns the caller, or 401 when anonymous and 403 when
// authenticated without the permission.
//
// The distinction matters: 401 tells a client to log in, 403 tells it not to
// bother.
func requirePermission(ctx context.Context, permission string) (auth.Principal, error) {
	p, err := requirePrincipal(ctx)
	if err != nil {
		return auth.Principal{}, err
	}
	if !p.Can(permission) {
		return auth.Principal{}, huma.Error403Forbidden("Your role does not permit this action.")
	}
	return p, nil
}

type requestContextKey struct{}

// withRequestContext binds the client's address and user agent, which every
// audit entry records.
func withRequestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := auth.RequestContext{
			IP:        clientIP(r),
			UserAgent: truncate(r.UserAgent(), 512),
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestContextKey{}, rc)))
	})
}

// requestContextFrom returns the ambient request detail bound to ctx.
func requestContextFrom(ctx context.Context) auth.RequestContext {
	rc, _ := ctx.Value(requestContextKey{}).(auth.RequestContext)
	return rc
}

// clientIP returns the peer address.
//
// X-Forwarded-For is deliberately ignored. It is trivially forged, and trusting
// it lets an attacker attribute their actions to any address they like — in an
// audit log, that is worse than recording nothing. When this runs behind a proxy
// we control, parse the header there and only for hops we trust.
func clientIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap()
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}
