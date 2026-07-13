# Architecture

A modular monolith in Go, backed by Postgres. `GUIDELINES.md` is the engineering contract; this document is what the system does and why.

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

Progress is a materialised roll-up recomputed in the transaction that changes a lesson, so it can never disagree with the rows it summarises. Finishing the last lesson completes the enrolment. Cancelling ends access but keeps progress: re-enrolling reactivates the original row, and the learner finds their place — unless the enrolment was *bought*, which cannot be cancelled at all. Handing the course back while keeping the money is not a thing a learner should be able to do to themselves, and a refund is the way out.

## Selling

The workspace is the merchant. The learner pays the school's own account, and Muallim takes a fee and never holds the money — collecting somebody else's takings makes us liable for their tax, their refunds and their disputes, and in most jurisdictions is money transmission, which is a licence rather than a feature.

`Gateway` is deliberately small: onboard, ask the account what it will do, open a hosted checkout. It stops there because that is where providers stop agreeing. How a payment is *confirmed* is a capability a driver declares — `Webhooker` for the ones that send a signed event (Stripe, and the fake), `Confirmer` for the ones whose truth is a question we have to ask. bKash sends no webhook at all: the learner returns to a callback whose query string proves nothing, and the only authority is a server-to-server execute. SSLCommerz sends an IPN, but its own documentation makes the validation API the authority anyway. A fat interface would have forced both of them to pretend to be Stripe.

Money is confirmed once, however it arrives. `Settle` moves an order only from `pending`, and reports whether it moved, so a webhook delivered three times enrols one learner. The enrolment commits in the same transaction as the paid order.

A refund is issued against the *payment*, not the checkout session that created it: several gateways forget the session the moment money moves. The gateway is called first and the database second — a row marked refunded against money that never moved is a learner locked out of a course they still paid for, which is the worse of the two failures — and the withdrawn enrolment commits with the refunded status.

Stripe needs no per-workspace secret: the platform's key plus a `Stripe-Account` header is the whole model. SSLCommerz and bKash have no such notion — each school is its own merchant — so their credentials sit in `payment_credentials`, sealed with AES-256-GCM under a key that is not in the database. Nothing reads a secret back out.

## Errors

Every error response is an [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) `application/problem+json` document carrying a correlation ID, including the ones the standard library would otherwise answer as plain text:

```console
$ curl -si localhost:8080/v1/nope | tail -1
{"title":"Not Found","status":404,"detail":"The requested resource does not exist.",
 "instance":"/v1/nope","correlation_id":"LE6OFPBDFF5AZKUQVWCUXTKPRL"}
```

A 5xx never leaks internals. The real error, with its stack, is logged against that correlation ID; the client receives only the ID.
