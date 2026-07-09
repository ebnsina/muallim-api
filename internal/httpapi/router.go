// Package httpapi is the transport layer. It is the only package that knows the
// service speaks HTTP: it registers routes, maps domain errors onto status
// codes, and renders RFC 9457 problem documents.
//
// Domain packages must never import this package.
package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

// Options carries what the transport layer needs from its caller in cmd/.
type Options struct {
	Version string
	Logger  *slog.Logger

	// CORSOrigins are the exact origins permitted to call this API from a browser.
	// Empty means no cross-origin request is allowed.
	CORSOrigins []string
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
	api := humago.New(mux, cfg)

	registerHealth(api, opts.Version)

	// Order matters. requestID is outermost so every log line and every rendered
	// problem document carries a correlation ID. cors sits outside problemResponse
	// so that error responses carry the headers too — without them a browser
	// blocks the response and the client sees an opaque network failure instead of
	// the 404 we sent. problemResponse sits outside recoverPanic so the 500 the
	// latter renders passes through untouched.
	handler := chain(mux,
		requestID,
		accessLog(opts.Logger),
		cors(opts.CORSOrigins),
		problemResponse(opts.Logger),
		recoverPanic(opts.Logger),
	)

	return handler, api
}
