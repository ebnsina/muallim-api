package tenant

import (
	"context"
	"fmt"
	"strings"

	"github.com/ebnsina/muallim-api/internal/platform/cache"
)

// Repository loads tenants. The interface is declared here, by its consumer, and
// satisfied by postgres.go in this package.
type Repository interface {
	ByHost(ctx context.Context, host string) (Tenant, error)
}

// Service resolves hosts to tenants.
//
// Resolution happens on every request to every endpoint, and the answer changes
// perhaps monthly, so it is cached in process. Cache misses for the same host are
// collapsed into one query: a cold cache behind a load balancer would otherwise
// issue one identical lookup per in-flight request.
type Service struct {
	repo  Repository
	cache *cache.Cache[Tenant]
}

// NewService returns a Service. A zero TTL disables caching, which is what tests
// want when they need every call to reach the repository.
func NewService(repo Repository, c *cache.Cache[Tenant]) *Service {
	return &Service{repo: repo, cache: c}
}

// ByHost resolves an inbound Host header to a tenant.
//
// A suspended tenant resolves successfully and is reported as ErrSuspended, so
// the transport layer can answer 403 rather than 404. Telling a suspended
// customer "not found" sends them to support with the wrong question.
func (s *Service) ByHost(ctx context.Context, host string) (Tenant, error) {
	key := normaliseHost(host)
	if key == "" {
		return Tenant{}, ErrNotFound
	}

	t, err := s.cache.GetOrLoad(ctx, key, func(ctx context.Context) (Tenant, error) {
		return s.repo.ByHost(ctx, key)
	})
	if err != nil {
		return Tenant{}, err
	}

	if !t.Active() {
		return t, fmt.Errorf("%w: %s", ErrSuspended, t.Status)
	}
	return t, nil
}

// Invalidate drops a host from the cache. The write path must call this whenever
// a tenant's subdomain, custom domain, or status changes; a cache that outlives
// its truth keeps a suspended tenant serving.
func (s *Service) Invalidate(host string) {
	s.cache.Invalidate(normaliseHost(host))
}

// normaliseHost lowercases and strips the port, so that "Acme.Lms.test:8080" and
// "acme.muallim.test" are one cache entry and one database lookup rather than two.
func normaliseHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))

	// Strip the port. A bracketed IPv6 literal has colons inside it, so cut at the
	// last colon only when it follows the closing bracket.
	if i := strings.LastIndexByte(host, ':'); i != -1 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	return strings.TrimSuffix(host, ".")
}
