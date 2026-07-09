# lms-api

Backend for a multi-tenant learning management system. A modular monolith in Go, backed by Postgres, publishing an OpenAPI 3.1 contract.

Its first client is [`lms-web`](../lms-web) (SvelteKit). A WordPress plugin, mobile apps, and an LTI tool are planned, and all of them consume this same API — which is why the spec is treated as a public interface rather than an implementation detail.

## Requirements

Go 1.26, Postgres 17, Docker (for local Postgres).

## Getting started

```bash
cp .env.example .env
make db-up          # start Postgres
make run            # serve on :8080
```

```bash
curl -s localhost:8080/v1/healthz | jq
open http://localhost:8080/docs      # interactive API reference
```

## The OpenAPI contract

The spec is generated from the Go types themselves, so it cannot drift from the implementation.

```bash
make spec           # writes bin/openapi.json
```

It is also served live at `/openapi.json`, `/openapi.yaml`, and rendered at `/docs`. `lms-web` generates its typed client from this document, so a breaking change here fails that build rather than production.

## Development

```bash
make check          # vet, format check, and race-enabled tests — what CI runs
make test
make lint
make fmt
make build          # binaries into bin/
```

## Layout

```
cmd/api                 HTTP server. `-dump-spec` prints the OpenAPI document.
internal/platform       config, logging, server — no domain knowledge
internal/httpapi        transport: routes, middleware, RFC 9457 problem documents
```

Domain packages land under `internal/` as they are built. The dependency rule is strict and enforced in review: `platform` imports nothing from the project, domain packages never import `httpapi` or each other, and only `httpapi` knows the service speaks HTTP.

## Errors

Every error response is an [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) `application/problem+json` document carrying a correlation ID, including the ones the standard library would otherwise answer as plain text:

```console
$ curl -si localhost:8080/v1/nope | tail -1
{"title":"Not Found","status":404,"detail":"The requested resource does not exist.",
 "instance":"/v1/nope","correlation_id":"LE6OFPBDFF5AZKUQVWCUXTKPRL"}
```

A 5xx never leaks internals. The real error, with its stack, is logged against that correlation ID; the client receives only the ID.

## Contributing

Read [GUIDELINES.md](GUIDELINES.md) first. It is the engineering contract, and a change that violates it should not merge.

## License

Not yet licensed.
