# Bundles

Course bundles: several courses grouped under one name and one price so a workspace
can sell them together. A self-contained modular-monolith domain like the rest — it
knows nothing about HTTP, references `courses(id)` by foreign key (never by import),
and is tenant-scoped with RLS behind every table. Money is bigint minor units +
currency, defaulting to BDT poisha, never a float; a bundle priced `0` is free, the
way a course with no `course_prices` row is free.

Granting a bundle enrols the learner in each of its courses. That step is **not** in
this package: the coordinator reads a bundle's course ids (`Service.CourseIDs`) and
calls enrol itself. Bundle exposes the pieces; it never imports enrol, catalog, or
commerce.

## Model

- **`bundles`** — `slug` (unique per tenant, lowercased), `name`, `description`
  (default `''`), `price_amount` (bigint minor units, default `0`, `>= 0`),
  `currency` (`char(3)`, default `BDT`).
- **`bundle_courses`** — a course's place in a bundle. `bundle_id` and `course_id`
  (both `ON DELETE CASCADE`), `position`. Unique on `(tenant_id, bundle_id,
  course_id)`, so a course appears in a bundle at most once.

Setting a bundle's courses replaces the whole list: delete-then-insert in one
transaction, with `unnest($1::uuid[]) WITH ORDINALITY` giving each course a dense
position from its index in the submitted order. A course named twice, or one that
does not exist, is refused (`ErrInvalid`) rather than half-applied.

`Get` loads a bundle and its ordered courses in exactly two queries — the bundle,
then its course refs batched with `= ANY` — so there is no N+1, asserted by a
`database.Counter` test. The bundle list is keyset-paginated, newest first
(`created_at DESC, id DESC`), over an index that covers the sort — no `Sort` node,
no `OFFSET`.

## Endpoints

Bearer auth. Reads need `course:read`; writes need `course:write`.

| Method | Path | Perm | Purpose |
| --- | --- | --- | --- |
| `GET` | `/v1/bundles` | `course:read` | Bundles, newest first. Keyset `cursor` + `has_more`. Omits `course_ids`. |
| `POST` | `/v1/bundles` | `course:write` | Create a bundle. Body: `slug`, `name`, `description?`, `price_amount?`, `currency?`. |
| `GET` | `/v1/bundles/{slug}` | `course:read` | A bundle with its ordered `course_ids`. |
| `PUT` | `/v1/bundles/{slug}` | `course:write` | Update `name`, `description`, and/or `price_amount`. A nil field is left alone. |
| `DELETE` | `/v1/bundles/{slug}` | `course:write` | Delete a bundle; its `bundle_courses` cascade. |
| `PUT` | `/v1/bundles/{slug}/courses` | `course:write` | Replace the ordered course list. Body: `course_ids` (ordered). |

There is no grant or checkout route here — the coordinator wires that with enrol.

## Errors

| Sentinel | Status | When |
| --- | --- | --- |
| `ErrNotFound` | 404 | No such bundle in this workspace. |
| `ErrDuplicate` | 409 | A bundle with that slug already exists. |
| `ErrInvalid` | 422 | Bad slug/name/price/currency, or a course list naming a missing or duplicated course. |
| `ErrInvalidPage` | 422 | The page cursor did not decode. |
