// Package tenant resolves an inbound host to the tenant it belongs to, and
// carries that tenant through the request context.
//
// It knows nothing about HTTP. The transport layer extracts a host, asks this
// package to resolve it, and binds the result to the context.
package tenant

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// Sentinel errors. The transport layer maps these to status codes; this package
// never imports net/http.
var (
	ErrNotFound  = errors.New("tenant: not found")
	ErrSuspended = errors.New("tenant: suspended")
)

// Status values.
const (
	StatusActive    = "active"
	StatusSuspended = "suspended"
	StatusCancelled = "cancelled"
)

// Tenant is one customer of the platform: a school, a creator, an organisation.
type Tenant struct {
	ID           uuid.UUID
	Subdomain    string
	CustomDomain string
	Name         string
	Status       string
}

// Active reports whether the tenant may serve requests.
func (t Tenant) Active() bool { return t.Status == StatusActive }

type ctxKey struct{}

// NewContext returns a context carrying t.
func NewContext(ctx context.Context, t Tenant) context.Context {
	return context.WithValue(ctx, ctxKey{}, t)
}

// FromContext returns the tenant bound to ctx.
//
// Callers that require a tenant should treat !ok as a programming error: the
// middleware guarantees one is present on every tenant-scoped route.
func FromContext(ctx context.Context) (Tenant, bool) {
	t, ok := ctx.Value(ctxKey{}).(Tenant)
	return t, ok
}

// ID returns the tenant's id, or uuid.Nil when ctx carries no tenant. Passing
// uuid.Nil to database.WithTenant is refused, so a missing tenant fails loudly
// rather than reading another tenant's rows.
func ID(ctx context.Context) uuid.UUID {
	t, ok := FromContext(ctx)
	if !ok {
		return uuid.Nil
	}
	return t.ID
}
