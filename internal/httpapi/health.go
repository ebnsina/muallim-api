package httpapi

import (
	"context"
	"net/http"

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
