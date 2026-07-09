# CLAUDE.md — `lms-api`

Go backend for a multi-tenant LMS. Read `GUIDELINES.md` before writing code; it is the authoritative engineering contract. This file is the short version plus the things that are easy to get wrong.

## What this repo is

An API-first, multi-tenant LMS backend. A **modular monolith** in Go, backed by Postgres. It publishes an OpenAPI 3.1 spec that is the contract for every client: `lms-web` (SvelteKit, sibling repo at `../lms-web`), and later a WordPress plugin, mobile apps, and an LTI tool.

Because those clients are separate, **the OpenAPI spec is a public interface.** Renaming an `OperationID` or narrowing a field breaks consumers you cannot see.

## Stack

Go 1.26 · Postgres 17 · Huma v2 (`humago` stdlib adapter, OpenAPI 3.1, RFC 9457 errors) · pgx v5 · goose (migrations) · River (Postgres-backed jobs — **no Redis**) · `log/slog` · argon2id + JWT.

## Commands

```bash
go build ./...
go vet ./...
gofmt -l .                  # must print nothing
go test ./... -race
go run ./cmd/api            # HTTP server
go run ./cmd/worker         # River job worker
go run ./cmd/migrate up     # migrations
docker compose up -d        # Postgres
```

## Rules that are easy to violate

**Never assume a library API.** Query Context7 (`resolve-library-id` → `query-docs`) before writing against any dependency. Verify with `go doc`. Training data lags releases.

**The dependency rule.** `platform` imports nothing from the project. Domain packages (`tenant`, `auth`, `catalog`, `assess`, `enroll`, `commerce`, `media`, `learn`, `comms`) may import `platform` but never `internal/httpapi`, and never each other — cross-domain needs go through an interface the *caller* defines, wired in `cmd/`. `internal/httpapi` imports domains; nothing imports it.

**Domain code does not know it is behind HTTP.** No domain package imports `net/http` or returns a `huma.StatusError`. Domains return sentinel errors (`ErrNotFound`, `ErrConflict`, `ErrForbidden`); `internal/httpapi` — and only that layer — maps them to status codes.

**Handle every error path.** `_ = err` is a rejection. 404, 401, 403, 409, 422, 429, 500 all render deliberately as `application/problem+json`. A 5xx never leaks internals: log the wrapped error with a correlation ID, return the ID.

**Every tenant-scoped query filters by `tenant_id`,** and an RLS policy backs it up. RLS is the net, not the primary control.

**Money is `bigint` minor units + `currency char(3)`.** Never a float.

**Enqueue jobs in the transaction that produced them** (`client.InsertTx`). That is the whole reason River is Postgres-backed.

**Anything over ~200ms or touching a third party is a job, not a handler.** Grading, transcoding, email, transcription, analytics. Jobs are retried, so jobs are idempotent.

**Webhooks arrive twice, out of order.** Deduplicate on a unique index over `(gateway, external_id)`.

## Git

Author every commit as `ebnsina <ebnsina.me@gmail.com>`, configured **per repo**:

```bash
git config user.name "ebnsina" && git config user.email "ebnsina.me@gmail.com"
```

Do **not** add a `Co-Authored-By: Claude` trailer, or any other identity. Remote uses the `github-es` SSH host alias (`git@github-es:ebnsina/lms-api.git`).

`docs/` and `data/` are gitignored and must never be committed — the public repo carries no plans, no roadmap, no secrets. `docs/plan.md` holds the product and architecture plan and is for local reference only.

Conventional, imperative commit subjects: `feat(catalog): add course prerequisites`.
