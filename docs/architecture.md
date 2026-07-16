# Architecture

A modular monolith in Go, backed by Postgres. `GUIDELINES.md` is the engineering contract; this document is what the system does and why.

## Layout

```
cmd/api                 HTTP server. `-dump-spec` prints the OpenAPI document.
cmd/migrate             goose migration runner
internal/platform       config, logging, server, database, cache, ratelimit, object store, secrets, mail, SMS
internal/tenant         host resolution, cached; context propagation
internal/auth           identity, sessions, RBAC, invitations, membership
internal/audit          append-only audit trail

internal/catalog        courses, topics, lessons, authoring
internal/taxonomy       course categories and tags, for browsing and filtering
internal/bundle         several courses grouped under one name and one price
internal/learnpath      an ordered track of courses, worked through in sequence
internal/media          what an author types about a video, made safe to put in a page
internal/enroll         enrolments, access rules, progress
internal/learn          a learner's own notes, highlights, and lesson questions
internal/assess         quizzes, questions, attempts, grading
internal/assign         assignments: work a learner uploads and a person marks
internal/grade          marks into a grade: the scale, and a course's roll-up
internal/certify        certificates, issued on completion and publicly verifiable
internal/gamify         points, badges, the leaderboard
internal/forum          community discussion: spaces, threads, replies
internal/chat           conversations and messages; WebSocket over Postgres LISTEN/NOTIFY
internal/liveclass      bring-your-own-link live sessions: a schedule and a URL
internal/notify         a person's in-app notifications
internal/comms          outbound email and SMS, enqueued in the deciding txn
internal/automation     a workspace's own rules for the mail it sends
internal/commerce       prices, orders, gateways, webhooks, refunds

internal/academics      institution spine: years, terms, classes, sections, subjects, students, guardians, attendance, timetable
internal/admissions     application intake; the admit step is orchestrated in httpapi
internal/exams          grading scales, exams, marks, computed report cards
internal/fees           fee structures, invoices, payments, student ledgers
internal/staff          the people who run the institution: teachers and the office
internal/notices        guardian broadcasts, fanned out to email/SMS in the posting txn
internal/hifz           madrasa Quran-memorization log: Sabaq, Sabqi, Manzil
internal/calendar       the academic calendar: holidays, exam dates, term boundaries
internal/library        the books a school holds and the loans it makes
internal/transport      routes, vehicles, and the students assigned to them
internal/hostel         buildings, rooms, and which student holds a bed
internal/payroll        salary structures and the payslips generated from them
internal/ledger         the school's own books: income and expense heads
internal/overview       institution dashboard read-model: counts and sums at a glance

internal/certdesign     certificate canvas designer: a jsonb layout, rendered client-side
internal/coursebuild    course-blueprint builder: a curriculum sketch as one document
internal/idcard         ID-card designer: positioned fields on a canvas

internal/httpapi        transport: routes, middleware, RFC 9457 problem documents
migrations/             embedded goose SQL
```

The parent-and-pupil portal is not a package: it is a handler group in `httpapi` that reads through `academics`, `fees` and `hifz`, gated by `portal:read` *and* an ownership check.

The dependency rule is strict and enforced in review: `platform` imports nothing from the project, domain packages never import `httpapi` or each other, and only `httpapi` knows the service speaks HTTP. A domain that needs a sibling declares the interface *it* wants and `cmd/` wires the far end in — `enroll` declares `Announcer` and gets `automation`, `Rewards` and gets `gamify`, `Prices` and gets `commerce`, and none of the four packages knows any of the others exists.

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

Roles map to permissions (`course:write`, `tenant:manage`), and unknown roles and unknown permissions both deny — a typo fails closed. The map is a map rather than a hierarchy on purpose: "an admin can do everything an instructor can" is true today and is exactly the assumption that quietly breaks when a role is added. `guardian` is the proof — it holds `portal:read` and nothing else, and sits under no one. Authentication happens in middleware; **authorisation happens in the handler**, so a new route is never accidentally public.

A permission is sometimes not enough on its own. `portal:read` is held by every guardian and every pupil in a workspace, so it establishes that the caller is a parent — not *whose* parent. The portal's handlers pair it with an ownership check (`academics.ChildStudent`), and one family reading another's fees is what the second gate prevents. See [modules/portal.md](modules/portal.md).

Every consequential action is written to an append-only `audit_log`, in the same transaction as the change it describes.

## Authoring

Courses are drafted, filled with topics and lessons, and then published — `course:write` and `course:publish` are separate permissions, because drafting and releasing are different acts. A course with no lessons cannot be published.

An unpublished course is invisible to anyone without authoring rights: they receive a 404, the same answer as for a course that does not exist. It is also never `public`-cacheable, so no CDN can store a draft and hand it to a stranger.

Positions are dense within a parent. A delete closes its gap in the same statement, and a reorder rewrites every position in one `UPDATE` — the sibling unique constraint is deferred so a full reversal is legal mid-statement. A submitted order must name every sibling exactly once, or it is refused rather than half-applied.

## Learning

Enrolment is a person's right to study a course, and it is not a payment — commerce will create enrolments, and so will a manual grant, a bulk import, and a free course. `source` records which, because "why does this person have access" is the first question support asks.

Who may read a lesson is decided by one pure function. An author reads anything, including their own draft. Nobody else reads anything in an unpublished course. A live enrolment reads everything. Otherwise a preview lesson is a free sample, readable by a stranger. A reader who may not see a lesson receives 404; a reader who simply needs to enrol receives 403.

The whole decision — access, content, and the reader's own progress — is **one query**, asserted by a test, because it runs on the hottest path in the product.

Enrolling in a course and finishing one are the two events a workspace may write its own rule about — a welcome, a congratulation, filled from `{{placeholders}}` checked when the rule is written rather than when a learner reads a broken sentence. (Only self-enrolment announces today; a grant, a purchase and an import do not.) The rule fires in the transaction that recorded the event, below the guard that decides whether anything actually happened, so re-clicking Enrol welcomes nobody twice. And it never fails the enrolment: a learner turned away from a course because a welcome email misfired is the worse of the two outcomes. See [modules/automations.md](modules/automations.md).

Progress is a materialised roll-up recomputed in the transaction that changes a lesson, so it can never disagree with the rows it summarises. Finishing the last lesson completes the enrolment. Cancelling ends access but keeps progress: re-enrolling reactivates the original row, and the learner finds their place — unless the enrolment was *bought*, which cannot be cancelled at all. Handing the course back while keeping the money is not a thing a learner should be able to do to themselves, and a refund is the way out.

## Selling

The workspace is the merchant. The learner pays the school's own account, and Muallim takes a fee and never holds the money — collecting somebody else's takings makes us liable for their tax, their refunds and their disputes, and in most jurisdictions is money transmission, which is a licence rather than a feature.

`Gateway` is deliberately small: onboard, ask the account what it will do, open a hosted checkout. It stops there because that is where providers stop agreeing. How a payment is *confirmed* is a capability a driver declares — `Webhooker` for the ones that send a signed event (Stripe, and the fake), `Confirmer` for the ones whose truth is a question we have to ask. bKash sends no webhook at all: the learner returns to a callback whose query string proves nothing, and the only authority is a server-to-server execute. SSLCommerz sends an IPN, but its own documentation makes the validation API the authority anyway. A fat interface would have forced both of them to pretend to be Stripe.

Money is confirmed once, however it arrives. `Settle` moves an order only from `pending`, and reports whether it moved, so a webhook delivered three times enrols one learner. The enrolment commits in the same transaction as the paid order.

A refund is issued against the *payment*, not the checkout session that created it: several gateways forget the session the moment money moves. The gateway is called first and the database second — a row marked refunded against money that never moved is a learner locked out of a course they still paid for, which is the worse of the two failures — and the withdrawn enrolment commits with the refunded status.

Some gateways do not finish a refund when they accept it. SSLCommerz answers `processing` and settles later, so a refund there is not done when the order is marked refunded — it is *started*. A capability, `RefundConfirmer`, marks the drivers whose refund is asynchronous, and a River job chases them: it snoozes on `processing` (a snooze does not spend an attempt, so a bank refund can be chased for days), records the confirmation when the money moves, and — the case that matters — writes `order.refund_failed` loudly when the gateway takes the refund back after accepting it, because that is a learner left without both the course and the money, and only a person can put it right. The job runs in the worker, and the worker builds only the gateways that have background work.

Stripe needs no per-workspace secret: the platform's key plus a `Stripe-Account` header is the whole model. SSLCommerz and bKash have no such notion — each school is its own merchant — so their credentials sit in `payment_credentials`, sealed with AES-256-GCM under a key that is not in the database. Nothing reads a secret back out.

## Lists

Every list is bounded, and every list that can outgrow a page carries a cursor. Those are two different promises, and for a while this system kept only the first: members, invitations and a learner's certificates each stopped at a cap and said nothing about stopping, which reads as an answer and is not one. A school with more members than the cap could not be listed to its end.

A cursor is the sort key of the last row of the page before, base64'd so a client cannot come to depend on a sort order we must stay free to change. The query fetches one row more than it needs — that extra row is how "is there more" is answered without a `COUNT(*)` that would read the whole table to say yes or no.

And a keyset with no index covering both the filter and the sort is an `OFFSET` lying about itself: Postgres reads every row, sorts it, and discards all but the page. `EXPLAIN` shows a Sort node, and that is the defect.

## Errors

Every error response is an [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) `application/problem+json` document carrying a correlation ID, including the ones the standard library would otherwise answer as plain text:

```console
$ curl -si localhost:8080/v1/nope | tail -1
{"title":"Not Found","status":404,"detail":"The requested resource does not exist.",
 "instance":"/v1/nope","correlation_id":"LE6OFPBDFF5AZKUQVWCUXTKPRL"}
```

A 5xx never leaks internals. The real error, with its stack, is logged against that correlation ID; the client receives only the ID.
