# Certificate Designer

A standalone visual-design tool for laying out a certificate on a canvas. It is
**deliberately independent of the `certify` domain** and its issued certificates:
this domain (`internal/certdesign`) stores only a reusable *design* — a canvas
orientation, a palette, an optional background image, and a list of positioned
elements — never a certificate awarded to a learner. A modular-monolith domain
like the rest: it knows nothing about HTTP and is tenant-scoped with RLS behind
its one table.

## Model

- **`certificate_designs`** — one saved layout. `name`, `orientation`
  (`landscape` | `portrait`, default `landscape`, CHECK-constrained), `accent`
  and `background_color` (free-text colour strings), a nullable `background_key`
  (the object-store key of an uploaded background image), and `layout` — a JSONB
  array of elements. Timestamps `created_at` / `updated_at`.

`layout` is a JSON array of elements, each:

```json
{ "id": "…", "kind": "title", "x": 0.1, "y": 0.2, "w": 0.8,
  "fontSize": 48, "fontWeight": 700, "color": "#111", "align": "center", "text": "…" }
```

`kind` is one of `title`, `learner`, `course`, `date`, `serial`, `signatory`,
`text`. `x`, `y`, and `w` are **fractions of the canvas in `[0,1]`**. The domain
validates that the layout parses to an array of at most 200 elements whose kinds
are known and whose `x`/`y`/`w` are in `[0,1]`; anything else is
`ErrInvalidLayout`. An unknown or invalid orientation is `ErrInvalidDesign`. The
API carries `layout` through as raw JSON — the designer owns its shape.

The listing is keyset-paginated, newest first (`created_at DESC, id DESC`), backed
by `certificate_designs_tenant_idx (tenant_id, created_at DESC, id DESC)` so the
cursor is a real keyset with no `Sort` node on the request path and no `OFFSET`.

## Background images

A background is uploaded straight to the object store, mirroring the catalog
course-thumbnail flow. `PresignBackground` signs a one-shot PUT for an image of the
declared size (PNG/JPEG/WebP, up to 8 MiB) under the design's own key prefix;
`ConfirmBackground` heads the object, checks the key belongs to this design, and
records `background_key`, deleting the image it replaces (best-effort). On read the
service signs a short-lived inline GET as `background_url`; a design with no
background simply omits it.

## Endpoints

All require `course:write` (a design tool is an authoring capability), bearer auth.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v1/certificate-designs` | Designs, newest first. Keyset `cursor` + `has_more`. |
| `POST` | `/v1/certificate-designs` | Create a design. Body: `name?`, `orientation?`, `accent?`, `background_color?`, `layout?`. |
| `GET` | `/v1/certificate-designs/{id}` | One design, with a signed `background_url` when set. |
| `PUT` | `/v1/certificate-designs/{id}` | Update. Every field optional; an omitted field is left unchanged. |
| `DELETE` | `/v1/certificate-designs/{id}` | Delete a design. |
| `POST` | `/v1/certificate-designs/{id}/background/uploads` | Presign a background upload. Body: `content_type`, `bytes`. |
| `POST` | `/v1/certificate-designs/{id}/background` | Record an uploaded background. Body: `key`. |

## Errors

| Sentinel | Status | When |
| --- | --- | --- |
| `ErrNotFound` | 404 | No design with that id in this workspace. |
| `ErrInvalidDesign` | 422 | Bad orientation, or an invalid background upload/key. |
| `ErrInvalidLayout` | 422 | Layout is not an array, has an unknown kind, or `x`/`y`/`w` outside `[0,1]`. |
| `ErrInvalidPage` | 422 | The page cursor is malformed. |
