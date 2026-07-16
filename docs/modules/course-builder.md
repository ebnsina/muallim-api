# Course Builder

A standalone visual course-structure designer. A **blueprint** is a curriculum
sketch — modules holding lessons — that an author shapes in a drag-and-drop editor
before (or instead of) committing to real catalogue content.

It is deliberately **independent of the `catalog` domain**: a blueprint references no
course, topic or lesson, and nothing in the catalogue references it. Like every other
domain here it knows nothing about HTTP and is tenant-scoped with RLS behind its
table.

## Model

- **`course_blueprints`** — one course-structure sketch. `name`, optional
  `description`, and a `structure` jsonb document. Timestamps and a `tenant_id`.

`structure` is a JSON array of **modules**, each holding **lessons**:

```json
[
  {
    "id": "m1",
    "title": "Foundations",
    "lessons": [
      { "id": "l1", "title": "Welcome", "kind": "video", "notes": "intro clip" },
      { "id": "l2", "title": "Check-in", "kind": "quiz", "notes": "" }
    ]
  }
]
```

A lesson `kind` is one of `video`, `text`, `quiz`, `assignment`, `file`. The whole
shape lives in one document so the editor saves it in a single write; the domain
validates the shape (an array of modules, each with a title and a lessons array whose
kinds are known) while the column only guarantees it is an array. An absent structure
defaults to `[]`.

The listing is keyset-paginated, newest first (`created_at DESC, id DESC`), backed by
`course_blueprints_tenant_idx` covering both filter and sort — no `Sort` node on the
request path, no `OFFSET`.

## Endpoints

All under `course:write`, bearer auth.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v1/course-blueprints` | List blueprints, newest first. Keyset `cursor` + `has_more`. |
| `POST` | `/v1/course-blueprints` | Create. Body: `name`, `description?`, `structure?`. |
| `GET` | `/v1/course-blueprints/{id}` | One blueprint. |
| `PUT` | `/v1/course-blueprints/{id}` | Update. Any omitted field is left unchanged; a supplied `structure` replaces the whole document. |
| `DELETE` | `/v1/course-blueprints/{id}` | Delete. 204 on success. |

## Errors

| Sentinel | Status | When |
| --- | --- | --- |
| `ErrNotFound` | 404 | No blueprint with that id in the tenant. |
| `ErrInvalidBlueprint` | 422 | Blank name, or a structure that is not an array of well-formed modules (missing title, unknown lesson kind). |
| `ErrInvalidPage` | 422 | The keyset cursor did not decode. |
