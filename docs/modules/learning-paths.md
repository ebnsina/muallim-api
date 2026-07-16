# Learning paths

A learning path is an ordered track of courses a learner works through in
sequence. A self-contained modular-monolith domain: it knows nothing about HTTP,
references the catalogue by `courses(id)` and nothing else, and is tenant-scoped
with RLS behind every table. It owns the ordered course list; the coordinator
wires per-course progress (from `enroll`) onto it to build the learner view.

## Model

- **`learning_paths`** — one track. `slug` (unique per tenant, URL-safe), `title`,
  `description`, and a `status` of `draft` or `published` — the same two-state
  visibility a course has.
- **`learning_path_courses`** — one course pinned to a path at a dense, zero-based
  `position`. `path_id` and `course_id` both `ON DELETE CASCADE`. A course appears
  in a path at most once (`unique (tenant_id, path_id, course_id)`), and a position
  is unique within a path (`unique (tenant_id, path_id, position)`,
  `DEFERRABLE INITIALLY DEFERRED` so a reorder is legal mid-statement).

Setting a path's courses replaces its whole membership: the submitted list must
name each course exactly once (a repeat is refused, not half-applied), and becomes
the new ordered set. Three statements for any number of courses — an existence
check, a clear, and one `unnest($3::uuid[]) WITH ORDINALITY` insert that assigns
dense positions. Loading a path with its courses is two queries whatever the count
(the path row, then its courses ordered by position) — no N+1.

The listing is keyset-paginated, newest first (`created_at DESC, id DESC`); the
unfiltered and filter-by-status shapes each have a covering index, so no `Sort`
node lands on the request path and no `OFFSET` is used.

## Endpoints

Reads require `course:read`, writes `course:write`. Bearer auth throughout.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v1/learning-paths` | Paths, newest first. Filter by `status`. Keyset `cursor` + `has_more`. |
| `POST` | `/v1/learning-paths` | Create a draft path. Body: `slug`, `title`, `description?`. |
| `GET` | `/v1/learning-paths/{slug}` | One path with its ordered `course_ids`. |
| `PUT` | `/v1/learning-paths/{slug}` | Update `title?`, `description?`, `status?`. |
| `DELETE` | `/v1/learning-paths/{slug}` | Delete a path (cascades its course rows). |
| `PUT` | `/v1/learning-paths/{slug}/courses` | Replace the ordered course list. Body: `course_ids` (ordered, each once). |

## Errors

| Sentinel | Status | When |
| --- | --- | --- |
| `ErrNotFound` | 404 | No path (or course id) resolves. |
| `ErrDuplicate` | 409 | The slug is already taken in this workspace. |
| `ErrIncompleteOrder` | 409 | A course order names a course more than once. |
| `ErrInvalid` | 422 | A blank/malformed slug or title, unknown status, or a course id that names no course. |
| `ErrInvalidPage` | 422 | The page cursor is not valid base64 JSON. |

## Coordinating with progress

`learnpath` exposes `CourseIDs(ctx, tenantID, pathID) ([]uuid.UUID, error)` — the
path's course ids in order. A coordinator in `cmd/` loads those, asks `enroll` for
each course's progress, and stitches the learner-facing view. `learnpath` never
imports `enroll`; the cross-domain need goes through the caller.
