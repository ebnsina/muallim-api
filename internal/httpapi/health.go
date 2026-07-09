package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// HealthOutput is the body of a liveness response.
type HealthOutput struct {
	Body struct {
		Status  string `json:"status" enum:"ok" doc:"Liveness indicator"`
		Version string `json:"version" doc:"Build version of the running service"`
	}
}

// registerHealth mounts the liveness probe.
//
// It reports only that the process is up and serving. It deliberately does not
// check Postgres: a liveness probe that fails on a transient database blip gets
// the container killed and turns a brief outage into a restart loop. Readiness —
// which does check dependencies — belongs on a separate endpoint.
func registerHealth(api huma.API, version string) {
	huma.Register(api, huma.Operation{
		OperationID: "get-health",
		Method:      http.MethodGet,
		Path:        "/v1/healthz",
		Summary:     "Liveness probe",
		Description: "Reports that the process is up and serving requests.",
		Tags:        []string{"System"},
	}, func(_ context.Context, _ *struct{}) (*HealthOutput, error) {
		out := &HealthOutput{}
		out.Body.Status = "ok"
		out.Body.Version = version
		return out, nil
	})
}

// ReadinessOutput reports whether the process can serve traffic.
type ReadinessOutput struct {
	Body struct {
		Status   string `json:"status" enum:"ready" doc:"Readiness indicator"`
		Database string `json:"database" enum:"up" doc:"Database reachability"`
	}
}

// registerReadiness mounts the readiness probe.
//
// Unlike liveness, this one checks Postgres: a replica that cannot reach the
// database should be removed from the load balancer, but must not be killed —
// restarting it will not bring the database back, and a restart loop across
// every replica turns a brief database blip into a total outage.
//
// It returns 503 rather than 500, because the correct client behaviour is to
// retry elsewhere, not to report a bug.
func registerReadiness(api huma.API, db Pinger) {
	huma.Register(api, huma.Operation{
		OperationID: "get-readiness",
		Method:      http.MethodGet,
		Path:        "/v1/readyz",
		Summary:     "Readiness probe",
		Description: "Reports whether the process and its dependencies can serve traffic.",
		Tags:        []string{"System"},
	}, func(ctx context.Context, _ *struct{}) (*ReadinessOutput, error) {
		if db == nil {
			return nil, huma.Error503ServiceUnavailable("Database is not configured.")
		}

		// Bounded: a probe that hangs is a probe that never removes a sick replica
		// from rotation.
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()

		if err := db.Ping(pingCtx); err != nil {
			return nil, huma.Error503ServiceUnavailable("Database is unreachable.")
		}

		out := &ReadinessOutput{}
		out.Body.Status = "ready"
		out.Body.Database = "up"
		return out, nil
	})
}
