# Course taxonomy

How the course catalogue is browsed and filtered: a course sits in at most one
**category** (a section of the catalogue) and carries any number of **tags**
(cross-cutting labels). A self-contained modular-monolith domain like the rest —
it knows nothing about HTTP, and it references the catalogue by `courses(id)`
through its own link tables. It never imports the `catalog` domain and never
touches the `courses` table; the course is the catalog domain's, the labels are
this one's. Every table is tenant-scoped with an RLS policy behind it.

## Model

- **`course_categories`** — a section of the catalogue. `name`, `slug`, unique on
  `(tenant_id, slug)`.
- **`course_tags`** — a cross-cutting label. `name`, `slug`, unique on
  `(tenant_id, slug)`.
- **`course_category_links`** — a course filed under a category. FKs to
  `courses(id)` and `course_categories(id)`, both `ON DELETE CASCADE`. Unique on
  `(tenant_id, course_id)` — **one category per course**, which is what makes
  setting a category a replace (`ON CONFLICT (tenant_id, course_id) DO UPDATE`).
- **`course_tag_links`** — a tag on a course. FKs to `courses(id)` and
  `course_tags(id)`, both `ON DELETE CASCADE`. Unique on
  `(tenant_id, course_id, tag_id)` — a course carries a tag at most once.

The slug is derived from the name when omitted (lowercased, non-alphanumerics
collapsed to hyphens). A slug clash in the workspace is `ErrDuplicate` (409).

Setting a course's tags is **replace-all in one statement**: a CTE deletes the
tags no longer wanted and the insert (`unnest($3::uuid[])`,
`ON CONFLICT DO NOTHING`) adds the new set, so a reader never sees the course with
no tags mid-update and an empty set simply clears them. Reading the courses in a
category or with a tag is one query each, covered by
`course_category_links (tenant_id, category_id, course_id)` and
`course_tag_links (tenant_id, tag_id, course_id)` — no `Sort` node, no N+1.

Both listings are keyset-paginated by `(name, id)`, covered by the unique slug
index's sibling ordering — pass the `next_cursor` from the previous page.

## Endpoints

Reads under `course:read`, writes under `course:write`; admin-only, bearer auth.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v1/course-categories` | Categories by name. Keyset `cursor` + `has_more`. |
| `POST` | `/v1/course-categories` | Add a category. Body: `name`, `slug?`. |
| `DELETE` | `/v1/course-categories/{id}` | Remove a category; its course links cascade away. |
| `GET` | `/v1/course-categories/{id}/courses` | The ids of the courses filed under a category, for the catalogue to filter. |
| `GET` | `/v1/course-tags` | Tags by name. Keyset `cursor` + `has_more`. |
| `POST` | `/v1/course-tags` | Add a tag. Body: `name`, `slug?`. |
| `DELETE` | `/v1/course-tags/{id}` | Remove a tag; its course links cascade away. |
| `GET` | `/v1/courses/{slug}/taxonomy` | A course's category (nullable) and tags. |
| `PUT` | `/v1/courses/{slug}/taxonomy` | Set them. Body: `category_id?` (null clears), `tag_ids` (replaces the whole set). |

The course is resolved by slug inside the domain — the set of a course's category
and tags commits in one transaction.

## Errors

| Sentinel | Status | When |
| --- | --- | --- |
| `ErrNotFound` | 404 | No such category, tag, or course slug in this workspace. |
| `ErrDuplicate` | 409 | The slug is already taken in this workspace. |
| `ErrInvalid` | 422 | Missing name, or a slug that normalises to empty / too long. |
| `ErrInvalidPage` | 422 | The page cursor did not decode. |
