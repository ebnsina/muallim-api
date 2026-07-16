# Live class sessions

Bring-your-own-link live meetings. An instructor schedules a session on a course and
pastes a meeting URL (Zoom, Meet, Jitsi — anything an `http(s)` link opens); enrolled
learners see it and join. There is no video infrastructure: this is scheduling plus a
stored link, gated by the course's own access model. A modular-monolith domain like
the rest — it knows nothing about HTTP, references courses and users by id, and is
tenant-scoped with RLS behind the table.

## Model

- **`live_sessions`** — one scheduled live class on a course. A `course_id` (FK, `ON
  DELETE CASCADE`), a `title`, a `description` (default `''`), a nullable `join_url`
  (a session may be scheduled before its link exists), `starts_at`, a nullable
  `ends_at` (a `CHECK` keeps it at or after `starts_at`), and a nullable
  `host_user_id` (FK, `ON DELETE SET NULL` — the row outlives the account).

The listing is keyset-paginated, newest/soonest start first (`starts_at DESC, id
DESC`), riding its covering index `(tenant_id, course_id, starts_at DESC, id DESC)` —
no `Sort` node on the request path, no `OFFSET`. The cursor is opaque base64 on the
wire (`next_cursor` + `has_more`).

Validation lives in the domain: a non-empty `title`, a `join_url` that is an
`http(s)` URL when present, and an `ends_at` at or after `starts_at`. A `PATCH` is
checked against the session it produces, so a backwards range can never be reached in
two requests.

## The access rule

Who may **read** a course's sessions is the course's own access model, resolved in
`internal/httpapi` (a domain package never imports a sibling):

- **Manage** (create, list-as-author, update, delete) requires `course:write`. The
  course is resolved with drafts visible (`Curriculum(slug, includeDrafts=true)`), so
  sessions can be scheduled before a course is published.
- **List** allows a caller who either holds `course:write` **or** has a live
  enrolment on the course (`enroll.IsEnrolled`). Anyone else gets **404, not 403** —
  the same rule that hides an unpublished course, so the answer does not leak who is
  enrolled where.

The meeting link is included in the view: seeing the session is what lets you join,
and the read is already gated. Listing responses are `private, no-store`.

## Endpoints

Bearer auth throughout.

| Method | Path | Perm | Purpose |
| --- | --- | --- | --- |
| `POST` | `/v1/courses/{slug}/live-sessions` | `course:write` | Schedule a session; host is you. Body: `title`, `description?`, `join_url?`, `starts_at`, `ends_at?`. |
| `GET` | `/v1/courses/{slug}/live-sessions` | `course:read` + gate | A course's sessions, soonest first. Keyset `cursor` + `has_more`. Gate: write-permission or enrolment, else 404. |
| `PATCH` | `/v1/live-sessions/{id}` | `course:write` | Change a session. Omitted fields left alone; `null` `join_url`/`ends_at` erases. |
| `DELETE` | `/v1/live-sessions/{id}` | `course:write` | Remove a session. |

## Errors

| Sentinel | Status | When |
| --- | --- | --- |
| `ErrNotFound` | 404 | No such session, an unknown course slug, or a reader with no access to the course. |
| `ErrInvalidSession` | 422 | An empty title, a non-`http(s)` join link, or an `ends_at` before `starts_at`. |
| `ErrInvalidPage` | 422 | The page cursor did not decode. |
