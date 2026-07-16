# The parent-and-pupil portal

A guardian signs in and sees their own child's day — attendance, fees,
memorisation — and nothing else; a pupil sees their own. It is not a domain
package: it is a handler group in `internal/httpapi/portal.go` that reads through
the very services the admin endpoints use (`academics`, `fees`, `hifz`). The
difference is not what is read. It is **who may call it, and about whom**.

**Every read is gated twice.** The `portal:read` permission proves the caller is a
guardian or a student at all; `academics.ChildStudent` proves that *this particular
student* is theirs. One check is not enough, and it is worth being precise about
why: `portal:read` is held by every guardian and every pupil in the workspace, so a
permission check alone would let any parent read any child's fees by pasting an id.
The ownership check is the one that keeps one family out of another's records, and
it is not optional — `portalChild` performs both, and every per-child handler goes
through `portalChild` rather than reading the path parameter itself.

A student who is not the caller's comes back as `ErrNotFound` — 404, never 403.
The portal admits existence to no one: a 403 would confirm that a student with that
id is enrolled at the school, which is precisely the fact a stranger is fishing for.

The child is named in the **path** (`/v1/portal/children/{id}/…`), and the id is
resolved against `ChildStudent` before anything is read. It is not a filter the
client is trusted with: the ownership query is what turns a submitted id into a
student, so an id belonging to somebody else's child never reaches a service. The
`/v1/portal/children` listing exists to give the client the ids it may legitimately
ask about — a chooser, not the authorisation.

## Identity

**A guardian's sign-in is an existing account linked to the guardian record — not
one the portal creates.** A guardian is a *contact* first (`guardians`: name,
phone, email, relation), and only some are ever given a login. Two separate acts:

1. The account is invited through the ordinary member flow with the `guardian`
   role. Joining a workspace is by invitation for a reason, and the portal is not
   an exception that quietly mints members.
2. An administrator (`academics:manage`) records which guardian record that account
   speaks for — `LinkGuardianUser`, one `UPDATE` of `guardians.user_id`.

The link's address is `POST /v1/students/{id}/guardians/{guardian_id}/account`, and
the `{id}` is checked: the handler reads that student's guardians and refuses a
guardian who is not among them, 404, the same answer the portal gives for any child
that is not yours. It buys no privilege — the caller may manage every student here
either way — but for a while the segment was decorative, and a link made under the
name of an unrelated child succeeded silently. An address that names a thing should
mean it.

`RoleGuardian` carries **`portal:read` and nothing else**: no course rights, no
management. A guardian who could edit is an admin, which this is deliberately not.
`RoleStudent` holds `portal:read` too, and reads their own record through the same
endpoints — the ownership query resolves it for them, so no second code path
exists to get wrong.

`students.user_id` and `guardians.user_id` are both nullable and neither is ever
assumed. A young child on a roll has no login; that is a school, not an error.

## The ownership query

`ChildrenFor` and `ChildStudent` are the whole of the model, and they are one
query each:

```sql
WHERE tenant_id = $1 AND (
  user_id = $2                       -- the pupil's own record
  OR id IN (SELECT sg.student_id FROM student_guardians sg
            JOIN guardians g ON g.id = sg.guardian_id AND g.tenant_id = sg.tenant_id
            WHERE sg.tenant_id = $1 AND g.user_id = $2)   -- a child they guard
)
```

`ChildStudent` is that same predicate with `AND id = $3`, so the ownership test and
the load are not two steps that could disagree — a student the user is not tied to
matches no row, and no row is `ErrNotFound`. `guardians_user_id_idx` on
`(tenant_id, user_id) WHERE user_id IS NOT NULL` covers the filter of this hot
read, partial because only linked guardians are ever matched. Tenant isolation is
in the predicate and RLS is behind it, as everywhere.

## Endpoints

All bearer auth. The listing needs `portal:read`; the three per-child reads need
`portal:read` **and** the student to be one of the caller's own.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v1/portal/children` | The students the signed-in guardian or pupil may see. |
| `GET` | `/v1/portal/children/{id}/attendance` | A child's attendance over a range, with a summary. Required `from`, `to` (dates). |
| `GET` | `/v1/portal/children/{id}/fees` | A child's invoices and what they still owe. |
| `GET` | `/v1/portal/children/{id}/hifz` | Where a child's Sabaq stands, and recent activity. `days` (1–365, default 30). |
| `POST` | `/v1/students/{id}/guardians/{guardian_id}/account` | Tie a guardian to the login that reads their child's portal. Body: `user_id`. **`academics:manage`**, not `portal:read` — an administrative act. 204. |

## Deliberately not here

Writes. The portal reads; it does not mark attendance, pay an invoice, or record a
Sabaq. It also owns no tables of its own — `guardians.user_id` (migration
`00059_portal.sql`) is the single column added for it, and everything else it shows
already belonged to `academics`, `fees`, or `hifz`. A read-model that copied those
rows could disagree with them.

## Errors

The handlers map through the sentinels of whichever domain answered
(`academicsError`, `feesError`, `hifzError`). The one worth naming:

| Sentinel | Status | When |
| --- | --- | --- |
| `academics.ErrNotFound` | 404 | No such student — *or* a student who is not the caller's. The two are answered identically on purpose. |
