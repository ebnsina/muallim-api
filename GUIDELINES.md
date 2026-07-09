# Engineering Guidelines — `lms-api`

These are rules, not suggestions. A change that violates one should not merge.

---

## 1. Never assume an API

Before writing code against any dependency — Huma, pgx, River, goose, a payment gateway, an AI provider — **look up its current documentation**. Model training data lags real releases, and a plausible-looking method that does not exist costs more to debug than it cost to check.

Use Context7 (`resolve-library-id` → `query-docs`) as the first stop for library documentation, ahead of a general web search. Verify a symbol exists before depending on it:

```bash
go doc github.com/danielgtaylor/huma/v2 Register
go list -m -f '{{.Version}}' github.com/riverqueue/river
```

If a doc and the compiler disagree, the compiler is right. Record the resolution in the PR.

---

## 2. Architecture

A **modular monolith**. One binary, hard internal boundaries.

```
cmd/api  cmd/worker  cmd/migrate     entrypoints — wiring only, no logic
internal/platform                    config, logging, db, server, errors
internal/<domain>                    tenant, auth, catalog, assess, enroll,
                                     commerce, media, learn, comms
internal/httpapi                     transport: routes, DTOs, middleware
migrations/                          goose SQL
```

### The dependency rule

- `platform` imports nothing from the project. It has no domain knowledge.
- A domain package may import `platform`. It must **never** import `internal/httpapi`.
- A domain package must **never** import a sibling domain package directly. If `commerce` needs something from `enroll`, `commerce` declares the interface it needs and takes it as a dependency; wiring in `cmd/` supplies the implementation.
- `internal/httpapi` imports domain packages. Nothing imports `internal/httpapi`.

Violations are how a modular monolith rots into a mud ball. Enforce it in review.

### Inside a domain package

```go
type Repository interface { ... }   // defined BY this package, for its own needs
type Service struct { repo Repository; ... }
```

`Service` holds business rules and owns transaction boundaries. `Repository` is an interface this package defines and a `postgres.go` in the same package implements. HTTP handlers touch `Service` only — never a repository, never a `*pgxpool.Pool`.

This is what makes tenancy swappable and unit tests cheap. It is not ceremony.

### Consumers define interfaces

Go interfaces belong to the caller, not the implementation. Do not export a `Storer` interface next to your concrete `PostgresStore` and expect callers to use it. Keep interfaces small — one to three methods is normal.

---

## 3. Modular, clean, maintainable code

- **Small, named things.** A function that needs a comment to explain *what* it does needs a better name or fewer responsibilities.
- **Accept interfaces, return structs.**
- **`context.Context` is the first parameter** of anything that touches I/O. Never store it in a struct.
- **No package-level mutable state.** No `init()` doing work. No global DB handle, logger, or config. Pass dependencies explicitly; construct them in `cmd/`.
- **Constructors validate.** `New*` returns `(T, error)` and refuses to build an unusable value. A constructed object is always ready to use.
- **Zero value or error.** No half-initialised structs.
- **Table-driven tests**, `t.Parallel()` where safe. Repository tests run against a real Postgres via `LMS_TEST_DATABASE_URL` (`make test-db`), and skip when it is unset. Mocking a database tests the mock: neither row-level security nor a query plan has an opinion about a fake. Each test seeds its own tenant, which keeps parallel tests isolated and exercises that isolation at the same time.

### Comments

Write a comment only to state a constraint the code cannot express — an invariant, a spec citation, a non-obvious ordering requirement. Never restate the next line. Never explain why your change is correct; that belongs in the commit message and dies with the PR.

Exported identifiers get a doc comment beginning with the identifier's name. That is the one exception.

---

## 4. Error handling — every case, deliberately

**No error is swallowed. No error path is unhandled.** `_ = err` is a review rejection unless accompanied by a comment naming the invariant that makes it impossible.

### Wrapping

Wrap with context as an error crosses a boundary; do not wrap when merely returning it up one level within the same function's concern.

```go
if err := s.repo.CourseByID(ctx, id); err != nil {
    return fmt.Errorf("catalog: load course %s: %w", id, err)
}
```

Lowercase, no trailing punctuation, no "failed to" (the word `error` is already in the signature). Always `%w`, never `%v`, when the caller may need `errors.Is`/`errors.As`.

### Sentinel errors and domain errors

Each domain package defines its own sentinels:

```go
var (
    ErrNotFound   = errors.New("not found")
    ErrConflict   = errors.New("conflict")
    ErrForbidden  = errors.New("forbidden")
)
```

The `internal/httpapi` layer — and **only** that layer — maps them to HTTP status codes. A domain package must never import `net/http` or return a `huma.StatusError`. Domain code does not know it is behind HTTP.

### The wire format: RFC 9457

Huma emits `application/problem+json` natively. Every error response is a Problem Details document.

| Situation | Status | Constructor |
| --- | --- | --- |
| Unknown route or missing resource | 404 | `huma.Error404NotFound` |
| Malformed body / failed validation | 422 | automatic from struct tags |
| Unauthenticated | 401 | `huma.Error401Unauthorized` |
| Authenticated, not permitted | 403 | `huma.Error403Forbidden` |
| Version/state conflict, duplicate | 409 | `huma.Error409Conflict` |
| Rate limited | 429 | `huma.Error429TooManyRequests` |
| Anything unexpected | 500 | `huma.Error500InternalServerError` |

**A 5xx never leaks internals.** Log the wrapped error with its stack and a correlation ID; return a generic detail plus that correlation ID to the client. The client can quote the ID in a support ticket; an attacker learns nothing.

Validation belongs in struct tags (`minLength`, `maxLength`, `format`, `enum`) so it lands in the OpenAPI spec and is enforced before a handler runs.

### Panics

A panic reaching the top of a request is a bug, not a control-flow tool. Recovery middleware catches it, logs it with the stack, returns 500 with a correlation ID, and keeps the server alive. Panic only for genuinely impossible states during startup, where crashing is correct.

### Contexts and cancellation

Respect `ctx.Done()`. A cancelled request must not leave a transaction open or a job half-enqueued. Distinguish `context.Canceled` (client hung up — log at debug) from `context.DeadlineExceeded` (we were too slow — log at warn and treat as a signal).

---

## 5. Database

- **Every tenant-scoped table has `tenant_id uuid not null`** and an RLS policy. Application code always filters by tenant explicitly; RLS is the net for the day someone forgets, not the primary control.
- **Migrations are forward-only, plain SQL, via goose.** Every migration has a tested `-- +goose Down`. Never edit a migration that has run anywhere but your laptop.
- **Money is `bigint` minor units plus `currency char(3)`.** Never `float`, never `numeric` for currency amounts.
- **Timestamps are `timestamptz`.** Store UTC. There is no such thing as a naive timestamp in this system.
- **Transactions live in `Service`,** not in `Repository`. A repository method participates in a transaction it is handed; it does not start one.
- **Enqueue jobs inside the transaction** that produced them (`client.InsertTx`). River is Postgres-backed precisely so that a job and the row that caused it commit together, or neither does. This eliminates an entire class of "the row exists but the email never sent" bug.
- **Idempotency for external events.** Gateway webhooks are deduplicated on a unique index over `(gateway, external_id)`. Assume every webhook arrives at least twice, out of order.

### Tenant binding

Tenant-scoped work goes through `db.WithTenant` or `db.WithTenantReadOnly`, always. Both open a transaction and bind `app.tenant_id` with `set_config(..., true)`, which is transaction-local — a session-level `SET` on a pooled connection is a cross-tenant data leak waiting for the right interleaving. Repositories receive a `pgx.Tx` and never touch the pool, so no query can skip the binding.

A missing tenant is `uuid.Nil`, and `WithTenant` refuses it. Forgetting a tenant therefore fails loudly instead of reading someone else's rows. `WithoutTenant` exists for the `tenants` table alone; every call site is a place to look twice.

Reads use `WithTenantReadOnly`. Postgres then refuses any write inside the transaction, which turns "this list endpoint accidentally mutates state" from something a reviewer must notice into a database error.

---

## 6. Performance

Performance is the competitive claim. LearnDash and Tutor are documented as slow — a large site has measured 120 seconds to list quizzes and 35 seconds to save one. Every rule below exists so we never earn that reputation.

### The N+1 problem

**Never issue a query inside a loop over rows.** Load the parents, collect their ids, and fetch all children in one query with `= ANY($1)`; stitch them together in Go with a map. `catalog.CurriculumFor` is the reference implementation: three queries for a course of any size.

This is not enforced by good intentions. `database.Counter` counts the queries issued under a context, and repository tests assert an exact number across fixtures of increasing size:

```go
counter := &database.Counter{}
ctx := database.WithCounter(t.Context(), counter)
_, _ = svc.Curriculum(ctx, tenantID, slug)
if counter.Count() != 3 { t.Errorf(...) }   // fails the moment a loop appears
```

A loop over twenty topics makes the count grow with the fixture and the test fails — at the size where it is cheap to notice. Any new endpoint that loads a tree gets one of these tests. `BEGIN`, `COMMIT`, and the tenant binding are excluded from the tally, so the number reads as the round trips a reviewer would expect.

### Pagination

**Keyset, never `OFFSET`.** `OFFSET 20000` makes Postgres read and discard twenty thousand rows, so the last page of a large catalog costs far more than the first, and a row inserted mid-scroll shifts every subsequent page. A keyset seeks straight to the position with the same index that satisfies the sort. Measured on 50,000 courses: the keyset page reads 21 rows, the `OFFSET` equivalent reads 20,021.

The cursor is opaque and base64-encoded so clients cannot depend on the sort key. Fetch `limit + 1` rows to answer "is there more" — never `COUNT(*)`, which scans every matching row on every request. **No list endpoint is unbounded**, ever; `MaxPageSize` is enforced and an out-of-range limit is a 422, not a silent truncation.

### Indexes

Every query a request path issues must be served by an index that covers **both the filter and the sort**, so the plan is an index scan with no sort node. Verify with `EXPLAIN (ANALYZE, COSTS OFF)` against realistic row counts — a sequential scan over 200 rows looks fine and is a time bomb. A `Sort` node or a `Seq Scan` on a hot path is a defect.

### Caching

Cache the smallest thing that is read on every request and changes rarely. Today that is exactly one thing: tenant resolution by host. Measured over 20 requests, the cache turns 20 tenant lookups into 1.

- `cache.Cache` collapses concurrent misses with single-flight. Without it a cold cache under load issues one identical query per in-flight request — a stampede, arriving exactly when the database can least afford it.
- **Negative results are cached too**, briefly. Otherwise a host nobody registered reaches the database on every request, which is a free denial-of-service primitive for anyone who can point DNS at us.
- **Invalidate on write, in the success branch of the transaction.** A cache that outlives its truth keeps a suspended tenant serving.
- Anything that must be consistent across replicas belongs in Postgres, not here.

At the HTTP layer, read endpoints carry an `ETag` and answer a matching `If-None-Match` with `304`. That saves bandwidth and client parsing, not database work — the handler has already run — so hot reads also carry a `Cache-Control` `max-age` so a fresh client does not revalidate at all. `public` is only ever correct for a response containing no user-specific data; anything reflecting who is asking is `private`.

### Bounds

`statement_timeout` is set on every connection: a query still running after five seconds is a bug or a missing index, and cancelling it protects every other request from queueing behind it. Queries slower than `LMS_DB_SLOW_QUERY_THRESHOLD` are logged at warn **with their statement text** — a slow-query log that omits the query cannot answer the only question it exists to answer. Arguments are never logged; that is where user data lives.

The pool is small on purpose. Postgres costs roughly 10 MB of backend memory per connection and degrades past a few hundred: a small pool with a queue beats a large one that thrashes the server.

---

## 7. Background work

Grading, transcoding, email, transcription, analytics rollups, and report generation are **jobs, not request handlers**. If an operation can exceed ~200ms or calls a third party, it belongs in River.

This is not a stylistic preference. Synchronous grading is the specific defect that makes LearnDash take 35 seconds to save a quiz.

Jobs must be **idempotent** — they will be retried. Make the work safe to repeat, or guard it with a uniqueness constraint.

---

## 8. HTTP & the OpenAPI contract

The generated OpenAPI 3.1 document **is** the contract with `lms-web` and every future client. Treat it as a public interface.

- Operations are registered with `huma.Register` and carry a stable `OperationID` — it becomes the generated client's method name. Renaming one is a breaking change.
- Request and response bodies are explicit Go structs. Never `map[string]any` on a public surface.
- Routes are versioned under `/v1`. Removing or narrowing a field, or renaming an `OperationID`, requires `/v2`.
- Every operation declares its error responses so they appear in the spec.
- List endpoints are cursor-paginated with a bounded, defaulted page size. No unbounded list, ever.

---

## 9. Security

- Argon2id for passwords. Never bcrypt, never SHA-anything.
- Access tokens are short-lived JWTs; refresh tokens are opaque, stored, and rotated on use.
- Authorisation is checked in `Service`, not in middleware alone. Middleware establishes *who you are*; the service decides *what you may do*.
- No secret in code, in a log line, or in an error message. Config comes from the environment; `.env` is gitignored and `.env.example` documents the keys.
- Parameterised queries only. String-concatenated SQL is a rejection.
- Rate-limit authentication and anything that sends mail or costs money.
- **Audit logging from day one.** FERPA and GDPR both require it, and adding it retroactively means backfilling history you never recorded.

---

## 10. Observability

`log/slog`, structured, JSON in production. Every log line inside a request carries `request_id` and `tenant_id`. Never log a password, token, or full card detail. Log at the boundary where you handle an error — not at every level you pass it through, which produces the same error five times and helps no one.

---

## 11. Git & commits

```bash
git config user.name  "ebnsina"
git config user.email "ebnsina.me@gmail.com"
```

Set **per repo**, never globally. Do **not** add a `Co-Authored-By` trailer or any other identity.

Remote uses the `github-es` SSH host alias: `git@github-es:ebnsina/lms-api.git`.

`docs/` and `data/` are gitignored and never committed.

Commits are conventional and imperative:

```
feat(catalog): add course prerequisites
fix(assess): grade essay attempts idempotently on retry
refactor(platform): extract tenant resolver from middleware
```

One logical change per commit. If the body needs to explain *why*, write it — that is what a commit body is for, and it is the right home for the reasoning that does not belong in a code comment.

---

## 12. Before you push

```bash
go build ./...
go vet ./...
gofmt -l .          # must print nothing
go test ./... -race
```

CI runs all of it plus `staticcheck`. A red build does not merge.
