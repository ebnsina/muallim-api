# Automations

A workspace's own rules for the mail it sends. Every message this system sent
before this domain existed was one the code chose — a verification, a reset, an
invitation, a digest. A school that wanted to welcome the people who enrol, or
congratulate the ones who finish, had to ask a person to remember, and nobody
does. A modular-monolith domain like the rest: it knows nothing about HTTP, and
neither it nor `enroll` imports the other.

A rule is a template and an event. The event says *when*; the subject and body say
*what*, carrying `{{placeholders}}` filled from what actually happened.

**A placeholder is checked when the rule is written, not when it is sent.** An
admin writing "Welcome to {{course}}" would otherwise discover the mistake through
a learner reading a broken sentence — the send is on an enrolment's transaction,
hours or weeks later, with nobody looking. So `checkPlaceholders` refuses a
template naming anything its event cannot fill, at the moment the author is still
looking at the form. At send time the opposite rule applies: a placeholder with no
value renders *empty* rather than leaving its own braces in the sentence, because
by then the author has been vetted and an empty gap is a smaller failure than the
machinery showing through.

## Model

- **`automation_rules`** — one "when this happens, send that". `event`, `subject`,
  `body`, and `enabled`.

`enabled` defaults to **false**. Off is a first-class state: an admin drafting a
welcome note must not have half a sentence sent to the next person who enrols.

The event set is closed twice — a `CHECK (event IN ('learner.enrolled',
'course.completed'))` on the table, and a Go catalogue (`Events`, `ValidEvent`,
`PlaceholdersFor`) that the write path validates against and the events endpoint
is built from. A rule naming an event nothing fires would silently never run, and
its author would have no way to discover that.

Placeholders are declared **per event**, not globally:

| Event | Fires when | Placeholders |
| --- | --- | --- |
| `learner.enrolled` | A learner joins a course: enrolling themselves, being granted a seat, taking a bundle, or paying for it. | `learner_name`, `course_title` |
| `course.completed` | A learner finishes the last lesson of a course. | `learner_name`, `course_title` |

`enroll` has five write paths and four of them announce: `Enrol` (self),
`Grant` (an administrator placing a learner), `GrantInTx` (the one `commerce`
calls when an order is paid) and `GrantCourses` (a bundle handed over). Each hangs
its announcement off the `created` flag `repo.Enrol` returns, so a learner
re-clicking Enrol — or a gateway redelivering a webhook — is welcomed once.

`BulkEnrol` (a cohort import) is the fifth, and announces nothing, deliberately:
it enrols hundreds of people in one statement, and naming each of them to fill a
template would be a query per learner in the one path built to have none. The
person who pasted the list is the one telling that cohort they are on the course.

This is the module's own history and worth keeping: for a while only `Enrol`
announced, so the learner who had just *paid* was the one guaranteed to hear
nothing. `TestEveryWayOntoACourseAnnouncesIt` enumerates the paths precisely
because the next one will be added the way those were — by calling `repo.Enrol`
and forgetting this.

`{{workspace}}` is deliberately not among them. Resolving the workspace's own name
where these events fire would mean a lookup the enrolment transaction has no other
reason to do, and a placeholder that renders blank is worse than one that never
existed.

The event is fixed at creation — `RulePatch` carries a subject, a body and an
enabled flag, and no event. A rule that fires on something else is a different
rule, and silently repointing one an admin already trusts is not an edit. Because
the event cannot move, an update's templates are checked against the event the rule
already has.

Two indexes. `automation_rules_firing_idx` on `(tenant_id, event) WHERE enabled` is
the one that matters: every announced enrolment and every completion asks that
question on a request path, so it must never scan — and it is partial because a
disabled rule is never fired and there is no reason to walk past it. `automation_rules_list_idx` on `(tenant_id, created_at DESC, id DESC)`
covers the admin's list. RLS with `FORCE ROW LEVEL SECURITY` sits behind the
application's `tenant_id` filter, as everywhere.

Every mutation writes an audit line (`automation.created`, `automation.updated`,
`automation.deleted`) in the same transaction as the change.

## Firing

Rules fire **in the transaction that recorded the event** — `Fire` takes the
caller's `pgx.Tx` and hands it to the mailer, so the email is enqueued on River in
the same commit. A welcome note for an enrolment that rolled back is a lie, and a
queue asked to send one has no way of knowing.

**Firing never fails the thing it is about.** A learner turned away from a course
because a welcome email misfired is a worse outcome than a welcome email nobody
sent, and the workspace can see the failure in its logs either way. `Fire` returns
its error for the caller to decide on; the `automationAnnouncer` adapter in
`cmd/api/notifiers.go` is where the decision is made — it logs and returns `nil`,
whether the learner could not be named, the course could not be named, or the queue
would not take the message. Nobody to send to is not an error either: a learner
with no address on file is a workspace that never collected one.

The fire sits **below the `created` guard**. `enroll.Enrol` announces only when the
repository reports a genuinely new row, and `CourseCompleted` only past the
`finished` guard — so a learner re-clicking Enrol is not welcomed twice, and
re-completing the last lesson congratulates nobody a second time. A workspace that
welcomed the same person twice is a workspace nobody trusts.

## Wiring

`enroll` declares the `Announcer` interface (`Enrolled`, `CourseCompleted`) and
`cmd/` satisfies it over this domain, so neither package imports the other and
enrolment has no business knowing what an email is. The adapter resolves what the
templates need — the learner's name and address from `auth`, the course's title
from `catalog` — both in the caller's transaction, so the values are the ones that
were true when the thing happened. A nil `Announcer` announces nothing, which is
what the spec-only build passes.

Symmetrically, this domain declares `Mailer` (`SendRendered`) and `cmd/` satisfies
it over `comms`. A nil mailer fires nothing.

## Endpoints

All under `tenant:manage`, bearer auth.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v1/automations/events` | The events a rule may fire on, and the placeholders each offers. The chooser is built from this — a client guessing at placeholders writes rules that render blank. |
| `GET` | `/v1/automations` | A workspace's rules, newest first. Bounded at `MaxRules` (100), not paginated: these are written by hand, a few per event, and a workspace at this cap has a problem no page of results would solve. |
| `POST` | `/v1/automations` | Write a rule. Body: `event`, `subject`, `body`, `enabled?`. 201. |
| `PUT` | `/v1/automations/{id}` | Edit a rule, or switch it on and off. Body: `subject?`, `body?`, `enabled?` — a nil field is left alone. |
| `DELETE` | `/v1/automations/{id}` | Delete a rule. 204. |

## Deliberately not here

The mail itself. This domain composes a subject and a body and hands them to a
`Mailer`; what an email is, how it is queued, and how it is delivered belong to
`comms`. There are no conditions beyond the event, no delay or schedule, and no
recipient other than the learner the event is about — each of those is a rule
engine, and none of them is what a school asking to welcome its learners needed.

## Errors

| Sentinel | Status | When |
| --- | --- | --- |
| `ErrNotFound` | 404 | No such rule in this workspace. |
| `ErrInvalid` | 422 | Nothing fires that event, the subject or body is blank, or a template names a placeholder the event cannot fill. |
