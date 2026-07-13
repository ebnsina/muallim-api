# CLAUDE.md — `muallim-api`

Go backend for a multi-tenant LMS. Read `GUIDELINES.md` before writing code; it is the authoritative engineering contract. This file is the short version plus the things that are easy to get wrong.

## What this repo is

An API-first, multi-tenant LMS backend. A **modular monolith** in Go, backed by Postgres. It publishes an OpenAPI 3.1 spec that is the contract for every client: `muallim-web` (SvelteKit, sibling repo at `../muallim-web`), and later a WordPress plugin, mobile apps, and an LTI tool.

Because those clients are separate, **the OpenAPI spec is a public interface.** Renaming an `OperationID` or narrowing a field breaks consumers you cannot see.

## Stack

Go 1.26 · Postgres 17 · Huma v2 (`humago` stdlib adapter, OpenAPI 3.1, RFC 9457 errors) · pgx v5 · goose (migrations) · River (Postgres-backed jobs — **no Redis**) · `log/slog` · argon2id + JWT.

## Commands

```bash
make db-create      # role + muallim/muallim_test databases on a local Postgres
make migrate        # apply migrations to both
make run            # HTTP server on :8080
make check          # vet, format check, race tests against a real Postgres
make test           # tests; database tests skip without MUALLIM_TEST_DATABASE_URL
make test-db        # every test, including the ones that need Postgres
make spec           # write bin/openapi.json — the contract for muallim-web
make seed           # a demo workspace with a demo account
make seed-huge      # ~1.1M rows, three workspaces — judge a page at the size it will be
```

The demo accounts all share one password, printed by `make seed`. `demo@` owns
the workspace, `marker@` has essays waiting.

The seeder writes assignments but no submissions: it holds a database connection
and cannot reach the object store, and a row pointing at a key with no object
behind it is a download that 404s. Upload a real file as `student@` instead —
`make storage-up` starts the MinIO it goes to.

## Rules that are easy to violate

**Docs are part of the change, not a follow-up.** A rule in CLAUDE.md that the code has outgrown is worse than no rule: it is a confident instruction to do the wrong thing, and the next person — or the next model — will follow it. So a convention that changes is rewritten in the *same* commit that changes it, a new endpoint appears in `docs/` when it ships, and `make spec` runs whenever the contract moves. A stale doc has no test, so nothing will ever fail to tell you.

**Comments are one line, two at most.** Say the one thing the code cannot. No multi-paragraph essays above a function, however tempting — trim to the load-bearing sentence.

**Never assume a library API.** Query Context7 (`resolve-library-id` → `query-docs`) before writing against any dependency. Verify with `go doc`. Training data lags releases.

**The dependency rule.** `platform` imports nothing from the project. Domain packages (`tenant`, `auth`, `catalog`, `assess`, `enroll`, `commerce`, `media`, `learn`, `comms`) may import `platform` but never `internal/httpapi`, and never each other — cross-domain needs go through an interface the *caller* defines, wired in `cmd/`. `internal/httpapi` imports domains; nothing imports it.

**Domain code does not know it is behind HTTP.** No domain package imports `net/http` or returns a `huma.StatusError`. Domains return sentinel errors (`ErrNotFound`, `ErrConflict`, `ErrForbidden`); `internal/httpapi` — and only that layer — maps them to status codes.

**Handle every error path.** `_ = err` is a rejection. 404, 401, 403, 409, 422, 429, 500 all render deliberately as `application/problem+json`. A 5xx never leaks internals: log the wrapped error with a correlation ID, return the ID.

**Every tenant-scoped query filters by `tenant_id`,** and an RLS policy backs it up. RLS is the net, not the primary control. Reach the database only through `db.WithTenant` / `db.WithTenantReadOnly`, which bind `app.tenant_id` transaction-locally — a session-level `SET` on a pooled connection is a cross-tenant leak. Repositories get a `pgx.Tx`, never the pool. The database role must not be a superuser, or RLS is silently bypassed.

**Never query inside a loop over rows.** Batch children with `= ANY($1)` and stitch with a map. `catalog.CurriculumFor` is the reference: three queries for a course of any size. Every tree-loading endpoint gets a `database.Counter` test asserting an exact query count across growing fixtures — that is what makes an N+1 a build failure rather than a customer's problem.

**Keyset pagination, never `OFFSET`.** Fetch `limit + 1` to detect a next page; never `COUNT(*)`. No list endpoint is unbounded — and *bounded is not the same as paginated*. `/v1/members`, `/v1/invitations` and a learner's certificates each capped at a few hundred rows with no cursor, which reads as an answer and is not one: a school with more members than the cap could not be listed to its end, and nothing said so. A list that can grow past a page gets a cursor (`auth.PageParams` / `certify.PageParams`, opaque base64, `next_cursor` + `has_more` on the wire), and a cursor gets an index covering the filter *and* the sort — otherwise the keyset is an `OFFSET` lying about itself, and `EXPLAIN` shows a Sort node to prove it.

**Every hot-path query needs an index covering both filter and sort.** Verify with `EXPLAIN (ANALYZE, COSTS OFF)` at realistic row counts. A `Sort` node or `Seq Scan` on a request path is a defect.

**Cache only what is read every request and changes rarely** — today, tenant resolution. Single-flight the misses, cache negatives briefly, invalidate on write. Read endpoints carry `ETag` and answer `If-None-Match` with 304; `public` caching only for responses with no user-specific data.

**Authentication and authorisation are different things.** Middleware establishes who you are; the handler decides what you may do (`requirePermission`). A middleware that authorises means each new route is protected only if someone remembers to list it. 401 = log in, 403 = don't bother. Unknown roles and unknown permissions both deny.

**An audit entry commits in the transaction of the thing it describes.** When the audited event is a *rejection* — failed login, detected token reuse — the transaction callback must return `nil` and the rejection is carried out in a variable. Returning the error rolls back the audit record you were obliged to keep.

**Never confirm an account exists.** Missing account, wrong password, and suspended membership are one error, in constant time (the unknown path hashes against a dummy digest). Registration claims an *unclaimed* workspace and nothing else — afterwards every attempt is refused identically, so it cannot be used to discover addresses. Joining is by invitation, and accepting one for an existing account requires that account's password: the invitation proves the workspace wants the address, not that the requester owns it.

**Rate-limit anything that verifies a credential.** Each Argon2id hash allocates 64 MiB; an unlimited login endpoint is a memory-exhaustion primitive. Key on the peer address per path, never on `X-Forwarded-For`.

**Every domain sentinel needs a case in its mapper**, or it renders as a 500 "unexpected error". `errors_test.go` asserts this for every sentinel, wrapped and unwrapped — add a line there in the same commit that adds a sentinel.

**Refresh tokens rotate; reuse revokes the family.** Distinguish *rotated away* (has a successor — theft) from *merely revoked* (logout or family sweep — just invalid). Both look like "session expired" to the client.

**RLS policies see other tables through those tables' own RLS.** A `NOT EXISTS (… tenant_id <> app_current_tenant())` clause is vacuously true — it grants what it appears to forbid. Cross-tenant invariants go in application code via `WithoutTenant`. And a `FORCE ROW LEVEL SECURITY` table denies every command it has no policy for, silently, by matching zero rows.

**Visibility is a query filter.** Unpublished content is excluded in SQL, from an authorisation decision, never from a request parameter. A reader who may not see it gets 404, not 403. Unpublished content is never `public`-cacheable — decide the directive from the resource's status, not from who asked.

**Ordering: dense positions, one-statement reorders.** Deletes close the gap in the same statement. Reorders use `unnest($1::uuid[]) WITH ORDINALITY`, never one UPDATE per row, and the sibling unique constraint is `DEFERRABLE` so a reversal is legal mid-statement. A submitted order must name every sibling exactly once, or it is refused rather than half-applied.

**The access rule is one pure function** (`enroll.decide`), enumerated in a table test. Zero value denies. Clause order is load-bearing — enrolment before preview, or a course with a preview lesson can never reach 100%. Load the entitlement in the same query as the resource: one query, asserted. Content whose visibility depends on the reader is `private, no-store`, never shared-cacheable.

**404 vs 403.** 404 when admitting existence would leak (a draft, a lesson in an invisible course). 403 when the resource is plainly visible and the answer is "enrol first" — there a 404 hides the button.

**Roll-ups are recomputed in the transaction that changes their inputs.** Not on read (a course page would count every lesson for every student), not in a trigger (action at a distance). `RecomputeProgress` is one statement, so the roll-up can never disagree with the rows it summarises.

**Money is `bigint` minor units + `currency char(3)`.** Never a float.

**The workspace sells; Muallim takes a fee and never holds the money.** The learner pays the school's own account, so the school is the merchant and owns its tax, its refunds and its disputes. Collecting a learner's money for somebody else's course would make us liable for all three, and in most jurisdictions is money transmission — a licence, not a feature. A course with no row in `course_prices` is free, which is what every course was before this existed.

**A gateway declares what it can do; it does not pretend.** `Gateway` is only the common ground — onboard, ask the account what it will do, open a hosted checkout. How money is *confirmed* is not common ground, and a fat interface would force every driver to lie about it. So confirmation and refunding are capabilities: `Webhooker` (Stripe, Fake — a signed event arrives), `Confirmer` (bKash, SSLCommerz — the truth is a question we *ask*: the learner returns to a callback and the driver goes back to the gateway to find out what really happened), `Refunder` (all four). The service type-asserts and answers `ErrUnsupported` rather than half-doing it. Four drivers: `Stripe` (Connect Standard, `application_fee_amount`, direct charges on the connected account), `SSLCommerz` and `Bkash` (Bangladesh; no platform account exists — each school is its own merchant), and `Fake` (a real driver with signed webhooks that takes no money, which is how the flow is exercised with no keys at all).

**A workspace's own gateway secrets are sealed before they touch the database.** Stripe needs none — the platform's key plus a `Stripe-Account` header is the whole model — but SSLCommerz and bKash have no such notion, so a store password and an app secret live in `payment_credentials`, AES-256-GCM under `MUALLIM_CREDENTIALS_KEY` (`platform/secret`). The key is not in the database, there is no endpoint that reads a secret back, and the audit entry records the *public* half only. A credential you can retrieve is a credential that leaks the day somebody's session does. No key, no SSLCommerz and no bKash: `cmd/` says so in a warning rather than half-starting them.

**Refunds are the only way out of a purchase, and the enrolment goes with them.** A learner cannot cancel an enrolment they bought — that handed the course back and kept the money, and re-enrolling asked them to buy what they already owned (`enroll.ErrPurchased`, 409). The workspace refunds: the gateway is called *first* and the database second, because a row marked refunded against money that never moved is a learner locked out of a course they still paid for. The status and the withdrawn enrolment commit in one transaction, and `WHERE status = 'paid'` makes the retry safe. A refund is issued against the *payment* (`orders.payment_external_id`, learned when the money moved), never against the checkout session — bKash refunds a trxID, Stripe refunds a payment intent, and neither will take a session id in its place.

**A webhook carries its tenant in signed metadata, and settles in one transaction.** It is unauthenticated by design — a gateway has no session with us — so the signature *is* the authentication, and the tenant comes from metadata the gateway echoed back inside the signed payload. It has to: `WithoutTenant` sees nothing under `FORCE ROW LEVEL SECURITY`. The order and the enrolment commit together, and `Settle`'s `WHERE status = 'pending'` is the whole of the idempotency — a gateway delivers the same event more than once, and the second delivery must enrol nobody twice.

**Enqueue jobs in the transaction that produced them** (`client.InsertTx`). That is the whole reason River is Postgres-backed.

**Anything over ~200ms or touching a third party is a job, not a handler.** Grading, transcoding, email, transcription, analytics. Jobs are retried, so jobs are idempotent.

**Webhooks arrive twice, out of order.** Deduplicate on a unique index over `(gateway, external_id)`.

## Git

Author every commit as `ebnsina <ebnsina.me@gmail.com>`, configured **per repo**:

```bash
git config user.name "ebnsina" && git config user.email "ebnsina.me@gmail.com"
```

Do **not** add a `Co-Authored-By: Claude` trailer, or any other identity. Remote uses the `github-es` SSH host alias (`git@github-es:ebnsina/muallim-api.git`).

`docs/` holds the installation, architecture, performance, and contract guides, and is committed. `docs/plan.md` and `data/` are gitignored and must never be committed — the product and architecture plan stays local.

Conventional, imperative commit subjects: `feat(catalog): add course prerequisites`.
