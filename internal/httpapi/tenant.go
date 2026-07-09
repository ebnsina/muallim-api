package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ebnsina/lms-api/internal/tenant"
)

// systemPaths serve the platform rather than a tenant: probes, the OpenAPI
// document, and the docs UI. They are reachable on any host.
var systemPaths = []string{"/v1/healthz", "/v1/readyz", "/openapi", "/docs", "/schemas/"}

// resolveTenant maps the inbound Host to a tenant and binds it to the request
// context, where domain services read it.
//
// Resolution is cached in process, so the common case costs a map lookup rather
// than a query. A host nobody has registered is cached too, briefly — otherwise
// anyone able to point DNS at us gets a free database query per request.
func resolveTenant(svc *tenant.Service, log *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isSystemPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			t, err := svc.ByHost(r.Context(), r.Host)
			switch {
			case err == nil:
				next.ServeHTTP(w, r.WithContext(tenant.NewContext(r.Context(), t)))

			case errors.Is(err, tenant.ErrNotFound):
				writeProblem(w, r, http.StatusNotFound, "No workspace is configured for this address.", log)

			case errors.Is(err, tenant.ErrSuspended):
				// Not a 404. A suspended customer who is told "not found" contacts
				// support with the wrong question.
				writeProblem(w, r, http.StatusForbidden, "This workspace is suspended.", log)

			default:
				log.ErrorContext(r.Context(), "resolve tenant",
					slog.String("error", err.Error()),
					slog.String("host", r.Host),
				)
				writeProblem(w, r, http.StatusInternalServerError, "", log)
			}
		})
	}
}

func isSystemPath(path string) bool {
	for _, p := range systemPaths {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}
