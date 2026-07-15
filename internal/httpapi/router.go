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

	"github.com/ebnsina/muallim-api/internal/academics"
	"github.com/ebnsina/muallim-api/internal/assess"
	"github.com/ebnsina/muallim-api/internal/assign"
	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/catalog"
	"github.com/ebnsina/muallim-api/internal/certify"
	"github.com/ebnsina/muallim-api/internal/commerce"
	"github.com/ebnsina/muallim-api/internal/enroll"
	"github.com/ebnsina/muallim-api/internal/exams"
	"github.com/ebnsina/muallim-api/internal/fees"
	"github.com/ebnsina/muallim-api/internal/forum"
	"github.com/ebnsina/muallim-api/internal/gamify"
	"github.com/ebnsina/muallim-api/internal/grade"
	"github.com/ebnsina/muallim-api/internal/learn"
	"github.com/ebnsina/muallim-api/internal/notify"
	"github.com/ebnsina/muallim-api/internal/platform/ratelimit"
	"github.com/ebnsina/muallim-api/internal/tenant"
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

	// WebBaseURL is where a gateway callback sends the learner when it is done with
	// them. A person redirected out of a payment app is owed a page, not a 204.
	WebBaseURL string

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

	// Academics is the institution layer — years, terms, classes, sections. Nil in a
	// deployment that only runs the LMS without the school-management surface.
	Academics *academics.Service

	// Exams is the assessment layer — grading scales, exams, marks, report cards. Nil
	// alongside Academics in an LMS-only deployment.
	Exams *exams.Service

	// Fees is the institutional billing layer — fee structures, invoices, payments.
	// Nil alongside Academics in an LMS-only deployment.
	Fees *fees.Service

	// Commerce may be nil: a deployment with no gateway configured sells nothing,
	// and every course in it is free — which is exactly what this product was
	// before there was such a thing as a price.
	Commerce *commerce.Service
	DB       Pinger

	// AuthLimiter throttles credential-verifying endpoints. Nil disables it, which
	// is what a transport test that hammers login wants.
	AuthLimiter *ratelimit.Limiter
}

// New builds the HTTP handler and the OpenAPI description of every route on it.
//
// The returned huma.API is exposed so that cmd/ can write the generated spec to
// disk. That spec is the contract with muallim-web and every future client; treat it
// as a public interface.
func New(opts Options) (http.Handler, huma.API) {
	mux := http.NewServeMux()

	cfg := huma.DefaultConfig("Muallim API", opts.Version)
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
	// The pricing is the commerce service, or nothing: a deployment that sells
	// nothing has no prices to show, and every course in it is free.
	var pricing coursePricing
	if opts.Commerce != nil {
		pricing = opts.Commerce
	}
	registerCatalog(api, opts.Catalog, opts.Enrol, pricing)
	registerCatalogWrites(api, opts.Catalog)
	registerCourseCopy(api, opts.Catalog)
	registerAnnouncements(api, opts.Catalog)
	registerAuthoring(api, opts.Catalog)
	registerPrerequisites(api, opts.Catalog)
	registerEnrolment(api, opts.Enrol)
	registerReviews(api, opts.Enrol)
	registerAnalytics(api, opts.Enrol)

	// Registered even when nil, because the spec is dumped from a handler built with
	// no services at all — and an endpoint missing from the contract is an endpoint
	// no client knows about. The handlers themselves refuse when there is nothing
	// behind them.
	registerCommerce(api, opts.Commerce, opts.WebBaseURL)

	// Assessment takes the enrolment service too: whether a person may see a quiz
	// is decided by whether they may see its lesson, and that rule lives there.
	registerAssessment(api, opts.Assess, opts.Enrol)
	registerBank(api, opts.Assess)

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
	registerAcademics(api, opts.Academics)
	registerSubjects(api, opts.Academics)
	registerStudents(api, opts.Academics)
	registerAttendance(api, opts.Academics)
	registerTimetable(api, opts.Academics)
	registerExams(api, opts.Exams)
	registerFees(api, opts.Fees)
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
