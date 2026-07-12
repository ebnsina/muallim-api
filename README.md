# muallim-api

Backend for a multi-tenant learning management system. A modular monolith in Go, backed by Postgres, publishing an OpenAPI 3.1 contract.

Its first client is [`muallim-web`](../muallim-web) (SvelteKit). A WordPress plugin, mobile apps, and an LTI tool are planned, and all of them consume this same API — which is why the spec is treated as a public interface rather than an implementation detail.

## Requirements

Go 1.26 and Postgres 17. Docker optional (`make db-up`) if you would rather not run Postgres locally.

The database role must **not** be a superuser: superusers bypass row-level security, which would silently disable tenant isolation.

## Getting started

```bash
cp .env.example .env
make db-create      # role + muallim/muallim_test databases
make migrate        # apply migrations to both
make run            # serve on :8080
```

```bash
curl -s localhost:8080/v1/healthz | jq
curl -s localhost:8080/v1/readyz  | jq          # also checks Postgres
curl -s -H 'Host: acme.muallim.test' localhost:8080/v1/courses | jq
open http://localhost:8080/docs                 # interactive API reference
```

## Multi-tenancy

A tenant is resolved from the request's `Host` — a subdomain, or a custom domain if one is configured. Resolution is cached in process behind a single-flight loader, so twenty requests cost one lookup rather than twenty.

Isolation is enforced twice. Application code always filters by `tenant_id`, and every tenant-scoped table carries a Postgres row-level security policy with `FORCE ROW LEVEL SECURITY`, which applies to the table owner too. The binding is transaction-local, so a pooled connection cannot carry one tenant's setting into the next request. With no tenant bound, every policy evaluates false and the query returns nothing: the failure mode is an empty page, never a leak.

## Identity and access

A user is global — one account across every workspace — and a *membership* binds them to a workspace with a role.

Registration **claims an unclaimed workspace**, and its claimant owns it. After that, joining is by invitation. That is not a restriction for its own sake: an address may already hold a global account from another workspace, and registration cannot link to it. It also means a claimed workspace answers every registration attempt identically, so the endpoint cannot be used to discover which addresses exist.

Accepting an invitation for an address that already has an account requires **that account's existing password**. The invitation proves the workspace wants the address; it does not prove the requester owns it.

Credential endpoints are rate-limited per address per path. Each Argon2id verification allocates 64 MiB by design, which is also what makes an unlimited login endpoint a memory-exhaustion primitive.

Passwords are Argon2id (RFC 9106 §4, second parameter set). Login is constant-time whether or not the account exists, because response latency must not answer "does this address have an account here?"

Access tokens are short-lived JWTs with the tenant inside the signature, so a token minted for one workspace cannot authenticate its bearer on another. Refresh tokens are opaque, stored only as a SHA-256 digest, and **rotate on every use**. Presenting a token that was already rotated away is evidence of theft: the whole session family is revoked, and the client is told only that its session expired.

Roles map to permissions (`course:write`, `tenant:manage`), and unknown roles and unknown permissions both deny — a typo fails closed. Authentication happens in middleware; **authorisation happens in the handler**, so a new route is never accidentally public.

Every consequential action is written to an append-only `audit_log`, in the same transaction as the change it describes.

## Authoring

Courses are drafted, filled with topics and lessons, and then published — `course:write` and `course:publish` are separate permissions, because drafting and releasing are different acts. A course with no lessons cannot be published.

An unpublished course is invisible to anyone without authoring rights: they receive a 404, the same answer as for a course that does not exist. It is also never `public`-cacheable, so no CDN can store a draft and hand it to a stranger.

Positions are dense within a parent. A delete closes its gap in the same statement, and a reorder rewrites every position in one `UPDATE` — the sibling unique constraint is deferred so a full reversal is legal mid-statement. A submitted order must name every sibling exactly once, or it is refused rather than half-applied.

## Learning

Enrolment is a person's right to study a course, and it is not a payment — commerce will create enrolments, and so will a manual grant, a bulk import, and a free course. `source` records which, because "why does this person have access" is the first question support asks.

Who may read a lesson is decided by one pure function. An author reads anything, including their own draft. Nobody else reads anything in an unpublished course. A live enrolment reads everything. Otherwise a preview lesson is a free sample, readable by a stranger. A reader who may not see a lesson receives 404; a reader who simply needs to enrol receives 403.

The whole decision — access, content, and the reader's own progress — is **one query**, asserted by a test, because it runs on the hottest path in the product.

Progress is a materialised roll-up recomputed in the transaction that changes a lesson, so it can never disagree with the rows it summarises. Finishing the last lesson completes the enrolment. Cancelling ends access but keeps progress: re-enrolling reactivates the original row, and the learner finds their place.

## Performance

The competitive claim is that this is fast, so the guarantees are tested rather than hoped for.

- **No N+1.** A curriculum of any size loads in three queries. `database.Counter` counts queries under a context, and a test asserts the exact count across fixtures of growing size — replace the batched fetch with a loop and the build goes red.
- **Keyset pagination.** Measured on 50,000 courses, a keyset page reads 21 rows where the `OFFSET` equivalent reads 20,021. Cursors are opaque; there is no `COUNT(*)`; no list endpoint is unbounded.
- **Indexes cover filter and sort**, so plans are index scans with no sort node.
- **Caching at both layers.** Tenant resolution is cached in process; read endpoints carry an `ETag` and answer `If-None-Match` with `304` and an empty body.
- **Bounded.** `statement_timeout` on every connection, a small pool, and a slow-query log that records the statement text — never its arguments.

## The OpenAPI contract

The spec is generated from the Go types themselves, so it cannot drift from the implementation.

```bash
make spec           # writes bin/openapi.json
```

It is also served live at `/openapi.json`, `/openapi.yaml`, and rendered at `/docs`. `muallim-web` generates its typed client from this document, so a breaking change here fails that build rather than production.

## Development

```bash
make check          # vet, format check, and race-enabled tests — what CI runs
make test
make lint
make fmt
make seed           # a demo workspace with a demo account and enough data to click around
make seed-huge      # the same at ~1.1M rows, to judge a page at the size it will be
make seed-test      # only the bare workspace the end-to-end tests need
make worker         # background jobs
make build          # binaries into bin/
```

## Demo accounts

`make seed` builds the `localhost` workspace and prints this table. Every account
shares one password.

| Email | Password | Role |
|---|---|---|
| `demo@muallim.test` | `demo-password-please-change` | owner |
| `instructor@muallim.test` | `demo-password-please-change` | instructor |
| `marker@muallim.test` | `demo-password-please-change` | instructor, with essays waiting |
| `student@muallim.test` | `demo-password-please-change` | student |

These are fixtures, not secrets: they exist only in a database `make seed` will
happily delete and rebuild, on a reserved `.test` domain that resolves nowhere.
`make seed -reset` drops the workspace and every account in it, which is why the
accounts you had before it ran are gone.

## CI

`.github/workflows/ci.yml` runs `make check`, `staticcheck`, `make spec`, and a build, against a real Postgres 17.

The `muallim` role it creates is `NOSUPERUSER NOBYPASSRLS`, and a step asserts as much before any test runs. A superuser bypasses row-level security, so every tenant-isolation test would pass against a database enforcing nothing — the most expensive kind of green build.

## Layout

```
cmd/api                 HTTP server. `-dump-spec` prints the OpenAPI document.
cmd/migrate             goose migration runner
internal/platform       config, logging, server, database, cache, ratelimit
internal/tenant         host resolution, cached; context propagation
internal/auth           identity, sessions, RBAC, invitations, membership
internal/audit          append-only audit trail
internal/catalog        courses, topics, lessons, authoring
internal/enroll         enrolments, access rules, progress
internal/httpapi        transport: routes, middleware, RFC 9457 problem documents
migrations/             embedded goose SQL
```

Remaining domain packages land under `internal/` as they are built. The dependency rule is strict and enforced in review: `platform` imports nothing from the project, domain packages never import `httpapi` or each other, and only `httpapi` knows the service speaks HTTP.

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
