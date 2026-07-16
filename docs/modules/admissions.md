# Admissions

Application intake. A prospective student (or their guardian) applies; the office
reviews and accepts or rejects; an accepted applicant is later *admitted*, which
creates a student. A modular-monolith domain like the rest — it knows nothing about
HTTP, references classes and students by id, and is tenant-scoped with RLS behind the
table.

**Intake enrols; it does not sign anybody up.** An application is not an account and
not a student. Submitting one records a row in the pending queue and nothing else.
The admit step that produces a student crosses into academics, so it is *not* in this
module: the coordinator creates the student and calls `MarkAdmitted` to close the
application, recording the resulting `student_id`. There is deliberately no
`POST /v1/admissions/{id}/admit` endpoint here.

## Model

- **`admission_applications`** — one prospective student's intake record.
  `applicant_name`, optional `guardian_name` / `guardian_phone` / `guardian_email`, an
  optional `grade_level_id` (the class applied for) and `dob`, a free-text `note`, a
  `status`, and — once admitted — the `student_id` produced. `submitted_at` and, after
  a decision, `decided_at`.

`status` moves `pending → accepted → admitted`, or `pending → rejected`. Each
transition is guarded in SQL by the status it requires (`WHERE status = 'pending'` to
accept or reject, `WHERE status = 'accepted'` to admit), so a repeat decision finds no
row and is refused rather than re-applied — a missing row is resolved to `ErrNotFound`
if the application is absent, `ErrNotPending` if it is merely no longer in that state.
`decided_at` is stamped on every transition. Each mutation writes an audit line in the
same transaction.

The intake board is keyset-paginated newest first (`submitted_at DESC, id DESC`), with
an index covering that sort, and a second `(tenant_id, status)` index for the status
filter — no `Sort` node on the request path, no `OFFSET`.

## Endpoints

All under `academics:manage`, admin-only, bearer auth.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v1/admissions` | Applications, newest first. Filter by `status`. Keyset `cursor` + `has_more`. |
| `POST` | `/v1/admissions` | Submit an application. Body: `applicant_name`, `guardian_name?`, `guardian_phone?`, `guardian_email?`, `grade_level_id?`, `dob?`, `note?`. |
| `GET` | `/v1/admissions/{id}` | One application. |
| `POST` | `/v1/admissions/{id}/accept` | Accept a pending application. |
| `POST` | `/v1/admissions/{id}/reject` | Reject a pending application. |

The admit step (create a student from an accepted application) is wired by the
coordinator via the service's `MarkAdmitted` method; it is not an endpoint in this
module.

## Errors

| Sentinel | Status | When |
| --- | --- | --- |
| `ErrNotFound` | 404 | No such application in this workspace. |
| `ErrNotPending` | 409 | The application is not in the status the decision requires (already decided, or not accepted when admitting). |
| `ErrInvalidApplication` | 422 | Missing applicant name. |
| `ErrInvalidPage` | 422 | The page cursor did not decode. |
