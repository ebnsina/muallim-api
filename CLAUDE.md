# CLAUDE.md — `lms-api`

Go backend for a multi-tenant LMS. Read `GUIDELINES.md` before writing code; it is the authoritative engineering contract. This file is the short version plus the things that are easy to get wrong.

## What this repo is

An API-first, multi-tenant LMS backend. A **modular monolith** in Go, backed by Postgres. It publishes an OpenAPI 3.1 spec that is the contract for every client: `lms-web` (SvelteKit, sibling repo at `../lms-web`), and later a WordPress plugin, mobile apps, and an LTI tool.

Because those clients are separate, **the OpenAPI spec is a public interface.** Renaming an `OperationID` or narrowing a field breaks consumers you cannot see.

## Stack

Go 1.26 · Postgres 17 · Huma v2 (`humago` stdlib adapter, OpenAPI 3.1, RFC 9457 errors) · pgx v5 · goose (migrations) · River (Postgres-backed jobs — **no Redis**) · `log/slog` · argon2id + JWT.

## Commands

```bash
make db-create      # role + lms/lms_test databases on a local Postgres
make migrate        # apply migrations to both
make run            # HTTP server on :8080
make check          # vet, format check, race tests against a real Postgres
make test           # tests; database tests skip without LMS_TEST_DATABASE_URL
make test-db        # every test, including the ones that need Postgres
make spec           # write bin/openapi.json — the contract for lms-web
```

## Rules that are easy to violate

**Never assume a library API.** Query Context7 (`resolve-library-id` → `query-docs`) before writing against any dependency. Verify with `go doc`. Training data lags releases.

**The dependency rule.** `platform` imports nothing from the project. Domain packages (`tenant`, `auth`, `catalog`, `assess`, `enroll`, `commerce`, `media`, `learn`, `comms`) may import `platform` but never `internal/httpapi`, and never each other — cross-domain needs go through an interface the *caller* defines, wired in `cmd/`. `internal/httpapi` imports domains; nothing imports it.

**Domain code does not know it is behind HTTP.** No domain package imports `net/http` or returns a `huma.StatusError`. Domains return sentinel errors (`ErrNotFound`, `ErrConflict`, `ErrForbidden`); `internal/httpapi` — and only that layer — maps them to status codes.

**Handle every error path.** `_ = err` is a rejection. 404, 401, 403, 409, 422, 429, 500 all render deliberately as `application/problem+json`. A 5xx never leaks internals: log the wrapped error with a correlation ID, return the ID.

**Every tenant-scoped query filters by `tenant_id`,** and an RLS policy backs it up. RLS is the net, not the primary control. Reach the database only through `db.WithTenant` / `db.WithTenantReadOnly`, which bind `app.tenant_id` transaction-locally — a session-level `SET` on a pooled connection is a cross-tenant leak. Repositories get a `pgx.Tx`, never the pool. The database role must not be a superuser, or RLS is silently bypassed.

**Never query inside a loop over rows.** Batch children with `= ANY($1)` and stitch with a map. `catalog.CurriculumFor` is the reference: three queries for a course of any size. Every tree-loading endpoint gets a `database.Counter` test asserting an exact query count across growing fixtures — that is what makes an N+1 a build failure rather than a customer's problem.

**Keyset pagination, never `OFFSET`.** Fetch `limit + 1` to detect a next page; never `COUNT(*)`. No list endpoint is unbounded.

**Every hot-path query needs an index covering both filter and sort.** Verify with `EXPLAIN (ANALYZE, COSTS OFF)` at realistic row counts. A `Sort` node or `Seq Scan` on a request path is a defect.

**Cache only what is read every request and changes rarely** — today, tenant resolution. Single-flight the misses, cache negatives briefly, invalidate on write. Read endpoints carry `ETag` and answer `If-None-Match` with 304; `public` caching only for responses with no user-specific data.

**Authentication and authorisation are different things.** Middleware establishes who you are; the handler decides what you may do (`requirePermission`). A middleware that authorises means each new route is protected only if someone remembers to list it. 401 = log in, 403 = don't bother. Unknown roles and unknown permissions both deny.

**An audit entry commits in the transaction of the thing it describes.** When the audited event is a *rejection* — failed login, detected token reuse — the transaction callback must return `nil` and the rejection is carried out in a variable. Returning the error rolls back the audit record you were obliged to keep.

**Never confirm an account exists.** Missing account, wrong password, and suspended membership are one error, in constant time (the unknown path hashes against a dummy digest). Registration claims an *unclaimed* workspace and nothing else — afterwards every attempt is refused identically, so it cannot be used to discover addresses. Joining is by invitation, and accepting one for an existing account requires that account's password: the invitation proves the workspace wants the address, not that the requester owns it.

**Rate-limit anything that verifies a credential.** Each Argon2id hash allocates 64 MiB; an unlimited login endpoint is a memory-exhaustion primitive. Key on the peer address per path, never on `X-Forwarded-For`.

**Every domain sentinel needs a case in its mapper**, or it renders as a 500 "unexpected error". `errors_test.go` asserts this for every sentinel, wrapped and unwrapped — add a line there in the same commit that adds a sentinel.

**Refresh tokens rotate; reuse revokes the family.** Distinguish *rotated away* (has a successor — theft) from *merely revoked* (logout or family sweep — just invalid). Both look like "session expired" to the client.

**RLS policies see other tables through those tables' own RLS.** A `NOT EXISTS (… tenant_id <> app_current_tenant())` clause is vacuously true — it grants what it appears to forbid. Cross-tenant invariants go in application code via `WithoutTenant`. And a `FORCE ROW LEVEL SECURITY` table denies every command it has no policy for, silently, by matching zero rows.

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
