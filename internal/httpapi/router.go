// Package httpapi is the transport layer. It is the only package that knows the
// service speaks HTTP: it registers routes, maps domain errors onto status
// codes, and renders RFC 9457 problem documents.
//
// Domain packages must never import this package.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/ebnsina/lms-api/internal/assess"
	"github.com/ebnsina/lms-api/internal/assign"
	"github.com/ebnsina/lms-api/internal/auth"
	"github.com/ebnsina/lms-api/internal/catalog"
	"github.com/ebnsina/lms-api/internal/certify"
	"github.com/ebnsina/lms-api/internal/enroll"
	"github.com/ebnsina/lms-api/internal/forum"
	"github.com/ebnsina/lms-api/internal/gamify"
	"github.com/ebnsina/lms-api/internal/grade"
	"github.com/ebnsina/lms-api/internal/learn"
	"github.com/ebnsina/lms-api/internal/notify"
	"github.com/ebnsina/lms-api/internal/platform/ratelimit"
	"github.com/ebnsina/lms-api/internal/tenant"
)

// Pinger reports whether a dependency is reachable. Declared here, by its
// consumer, so this package does not depend on the database package.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Options carries what the transport layer needs from its caller in cmd/.
type Options struct {
	Version string
	Logger  *slog.Logger

	// CORSOrigins are the exact origins permitted to call this API from a browser.
	// Empty means no cross-origin request is allowed.
	CORSOrigins []string

	// Services. All may be nil when the handler is built only to emit the OpenAPI
	// document, since no handler runs in that case. They must be non-nil before
	// the handler serves a request.
	Tenants *tenant.Service
	Catalog *catalog.Service
	Auth    *auth.Service
	Enrol   *enroll.Service
	Assess  *assess.Service
	Assign  *assign.Service
	Grades  *grade.Service
	Certify *certify.Service
	Learn   *learn.Service
	Notify  *notify.Service
	Forum   *forum.Service
	Gamify  *gamify.Service
	DB      Pinger

	// AuthLimiter throttles credential-verifying endpoints. Nil disables it, which
	// is what a transport test that hammers login wants.
	AuthLimiter *ratelimit.Limiter
}

// New builds the HTTP handler and the OpenAPI description of every route on it.
//
// The returned huma.API is exposed so that cmd/ can write the generated spec to
// disk. That spec is the contract with lms-web and every future client; treat it
// as a public interface.
func New(opts Options) (http.Handler, huma.API) {
	mux := http.NewServeMux()

	cfg := huma.DefaultConfig("LMS API", opts.Version)
	cfg.Info.Description = "Multi-tenant learning management system."

	// Declaring the scheme puts an Authorize button on the docs page and marks
	// every `Security:` operation as protected in the generated client.
	cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"bearer": {
			Type:         "http",
			Scheme:       "bearer",
			BearerFormat: "JWT",
			Description:  "A short-lived access token from /v1/auth/login.",
		},
	}

	api := humago.New(mux, cfg)

	registerHealth(api, opts.Version)
	registerReadiness(api, opts.DB)
	registerAuth(api, opts.Auth)
	registerMembers(api, opts.Auth)
	registerCatalog(api, opts.Catalog)
	registerCatalogWrites(api, opts.Catalog)
	registerAnnouncements(api, opts.Catalog)
	registerAuthoring(api, opts.Catalog)
	registerPrerequisites(api, opts.Catalog)
	registerEnrolment(api, opts.Enrol)
	registerReviews(api, opts.Enrol)
	registerAnalytics(api, opts.Enrol)

	// Assessment takes the enrolment service too: whether a person may see a quiz
	// is decided by whether they may see its lesson, and that rule lives there.
	registerAssessment(api, opts.Assess, opts.Enrol)

	// Assignments take the enrolment service for the same reason: whether a person
	// may see one is whether they may see its lesson.
	registerAssignments(api, opts.Assign, opts.Enrol)
	registerGrades(api, opts.Grades)
	registerCertificates(api, opts.Certify)
	registerNotes(api, opts.Learn)
	registerHighlights(api, opts.Learn)
	registerCourseAnnotations(api, opts.Learn)
	registerQA(api, opts.Learn)
	registerNotifications(api, opts.Notify)
	registerForum(api, opts.Forum)
	registerGamification(api, opts.Gamify)

	// Order matters, outermost first.
	//
	//   requestID          every log line and problem document carries a correlation ID
	//   accessLog          observes the final status, after every rewrite below
	//   cors               error responses need the headers too, or a browser hides them
	//   throttle           rejects before any Argon2 hash is computed, which is the point
	//   etag               hashes the body a handler produced, before it is committed
	//   withRequestContext client address and user agent, for the audit trail
	//   resolveTenant      binds the tenant that domain services read from context
	//   authenticate       verifies a bearer token; must run after resolveTenant so it
	//                      can reject a token minted for a different workspace
	//   problemResponse    rewrites any non-problem 4xx/5xx into RFC 9457
	//   recoverPanic       innermost, so a panic in a handler is caught and rendered
	mw := []middleware{
		requestID,
		accessLog(opts.Logger),
		cors(opts.CORSOrigins),
	}

	if opts.AuthLimiter != nil {
		mw = append(mw, throttle(opts.AuthLimiter, opts.Logger))
	}

	mw = append(mw, etag(opts.Logger), withRequestContext)

	// Omitted when there is no tenant service — building the handler to emit the
	// OpenAPI document, or a transport test that exercises no tenant-scoped route.
	if opts.Tenants != nil {
		mw = append(mw, resolveTenant(opts.Tenants, opts.Logger))
	}
	if opts.Auth != nil {
		mw = append(mw, authenticate(opts.Auth, opts.Logger))
	}

	mw = append(mw, problemResponse(opts.Logger), recoverPanic(opts.Logger))

	return chain(mux, mw...), api
}
