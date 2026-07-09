package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// ContentTypeProblem is the RFC 9457 media type. Huma emits it for errors raised
// inside an operation; problemResponse emits it for everything that never
// reaches one, so a client sees exactly one error shape.
const ContentTypeProblem = "application/problem+json"

// problem is an RFC 9457 Problem Details document. Its field names mirror the
// model Huma generates, so clients parse one shape regardless of where in the
// stack an error originated.
type problem struct {
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`

	// Instance identifies the specific occurrence: the request path.
	Instance string `json:"instance,omitempty"`

	// CorrelationID lets a user quote an identifier in a support ticket while the
	// underlying error stays in our logs and out of the response.
	CorrelationID string `json:"correlation_id,omitempty"`
}

// writeProblem renders an RFC 9457 document. It never includes the underlying
// error: 5xx detail is generic by construction, and the caller is responsible
// for having logged the real cause against the same correlation ID.
//
// Headers already set on w — notably Allow on a 405 — are preserved.
func writeProblem(w http.ResponseWriter, r *http.Request, status int, detail string, log *slog.Logger) {
	if detail == "" {
		detail = detailFor(status)
	}

	p := problem{
		Title:         http.StatusText(status),
		Status:        status,
		Detail:        detail,
		Instance:      r.URL.Path,
		CorrelationID: RequestIDFrom(r.Context()),
	}

	w.Header().Set("Content-Type", ContentTypeProblem)
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(p); err != nil {
		// The status line and headers are already on the wire, so there is no way
		// to signal this to the client. Record it and move on.
		log.ErrorContext(r.Context(), "encode problem response",
			slog.Any("error", err),
			slog.Int("status", status),
		)
	}
}

// detailFor supplies human-readable prose for the statuses the standard library
// answers on our behalf, and a safe generic string for anything else.
func detailFor(status int) string {
	switch status {
	case http.StatusNotFound:
		return "The requested resource does not exist."
	case http.StatusMethodNotAllowed:
		return "That method is not allowed on this resource. See the Allow header."
	case http.StatusRequestEntityTooLarge:
		return "The request body is too large."
	default:
		if status >= http.StatusInternalServerError {
			return "An unexpected error occurred. Quote the correlation ID when reporting this."
		}
		return http.StatusText(status)
	}
}
