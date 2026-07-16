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
	"github.com/ebnsina/muallim-api/internal/admissions"
	"github.com/ebnsina/muallim-api/internal/assess"
	"github.com/ebnsina/muallim-api/internal/assign"
	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/bundle"
	"github.com/ebnsina/muallim-api/internal/calendar"
	"github.com/ebnsina/muallim-api/internal/catalog"
	"github.com/ebnsina/muallim-api/internal/certdesign"
	"github.com/ebnsina/muallim-api/internal/certify"
	"github.com/ebnsina/muallim-api/internal/chat"
	"github.com/ebnsina/muallim-api/internal/commerce"
	"github.com/ebnsina/muallim-api/internal/coursebuild"
	"github.com/ebnsina/muallim-api/internal/enroll"
	"github.com/ebnsina/muallim-api/internal/exams"
	"github.com/ebnsina/muallim-api/internal/fees"
	"github.com/ebnsina/muallim-api/internal/forum"
	"github.com/ebnsina/muallim-api/internal/gamify"
	"github.com/ebnsina/muallim-api/internal/grade"
	"github.com/ebnsina/muallim-api/internal/hifz"
	"github.com/ebnsina/muallim-api/internal/hostel"
	"github.com/ebnsina/muallim-api/internal/idcard"
	"github.com/ebnsina/muallim-api/internal/learn"
	"github.com/ebnsina/muallim-api/internal/learnpath"
	"github.com/ebnsina/muallim-api/internal/ledger"
	"github.com/ebnsina/muallim-api/internal/library"
	"github.com/ebnsina/muallim-api/internal/liveclass"
	"github.com/ebnsina/muallim-api/internal/notices"
	"github.com/ebnsina/muallim-api/internal/notify"
	"github.com/ebnsina/muallim-api/internal/overview"
	"github.com/ebnsina/muallim-api/internal/payroll"
	"github.com/ebnsina/muallim-api/internal/platform/ratelimit"
	"github.com/ebnsina/muallim-api/internal/staff"
	"github.com/ebnsina/muallim-api/internal/taxonomy"
	"github.com/ebnsina/muallim-api/internal/tenant"
	"github.com/ebnsina/muallim-api/internal/transport"
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

	// Staff is the people layer — teachers and the office. Nil alongside Academics in
	// an LMS-only deployment.
	Staff *staff.Service

	// Notices posts guardian broadcasts. Nil alongside Academics in an LMS-only
	// deployment.
	Notices *notices.Service

	// Hifz is the madrasa memorization log. Nil in a non-madrasa or LMS-only
	// deployment.
	Hifz *hifz.Service

	// Overview is the institution dashboard read-model. Nil alongside Academics in an
	// LMS-only deployment.
	Overview *overview.Service

	// Library lends books. Transport rides students to school. Hostel boards them.
	// Payroll pays the staff. Ledger keeps the school's own books. Calendar holds
	// its year. All nil in an LMS-only deployment.
	Library   *library.Service
	Transport *transport.Service
	Hostel    *hostel.Service
	Payroll   *payroll.Service
	Ledger    *ledger.Service
	Calendar  *calendar.Service

	// CertDesign and CourseBuild are the two standalone builders — a certificate
	// canvas and a course blueprint. Independent of certify and catalog.
	CertDesign  *certdesign.Service
	CourseBuild *coursebuild.Service

	// Admissions takes applications and, with Academics, admits them into students.
	Admissions *admissions.Service

	// IDCard designs student and staff identity cards.
	IDCard *idcard.Service

	// LiveClass schedules bring-your-own-link meetings on a course.
	LiveClass *liveclass.Service

	// Taxonomy tags and categorises courses; Bundle groups them for sale; LearnPath
	// sequences them into a track; Chat is real-time messaging.
	Taxonomy  *taxonomy.Service
	Bundle    *bundle.Service
	LearnPath *learnpath.Service
	Chat      *chat.Service

	// ChatHub carries chat's realtime layer — the WebSocket route + LISTEN/NOTIFY
	// fan-out. Nil disables realtime (REST chat still works). Built in cmd/, which
	// also owns starting its listener goroutine.
	ChatHub *ChatHub

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
	registerCatalog(api, opts.Catalog, opts.Enrol, pricing, opts.Taxonomy)
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
	registerStaff(api, opts.Staff)
	registerNotices(api, opts.Notices)
	registerHifz(api, opts.Hifz)
	registerOverview(api, opts.Overview)
	registerLibrary(api, opts.Library)
	registerTransport(api, opts.Transport)
	registerHostel(api, opts.Hostel)
	registerPayroll(api, opts.Payroll)
	registerLedger(api, opts.Ledger)
	registerCalendar(api, opts.Calendar)
	registerPortal(api, opts.Academics, opts.Fees, opts.Hifz)
	registerGuardianLink(api, opts.Academics)
	registerCertDesigns(api, opts.CertDesign)
	registerCourseBlueprints(api, opts.CourseBuild)
	registerAdmissions(api, opts.Admissions)
	registerAdmissionsAdmit(api, opts.Admissions, opts.Academics)
	registerIDCards(api, opts.IDCard)
	registerLiveSessions(api, opts.LiveClass, opts.Catalog, opts.Enrol)
	registerTaxonomy(api, opts.Taxonomy)
	registerBundles(api, opts.Bundle)
	registerBundleGrant(api, opts.Bundle, opts.Enrol)
	registerLearningPaths(api, opts.LearnPath)
	registerLearnPathProgress(api, opts.LearnPath, opts.Enrol)
	registerChat(api, opts.Chat, opts.Enrol, opts.ChatHub)
	registerChatWS(api, mux, opts.ChatHub)
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
