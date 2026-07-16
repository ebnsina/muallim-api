# ID Card Designer

A standalone visual-design tool for laying out a student or staff ID card on a
canvas. This domain (`internal/idcard`) stores only a reusable *template* — a
subject, a canvas orientation, a palette, an optional background image, and a
list of positioned fields — never a card issued to a real person: the web renders
a card for a person by filling the template's fields. A modular-monolith domain
like the rest: it knows nothing about HTTP and is tenant-scoped with RLS behind
its one table.

## Model

- **`id_card_templates`** — one saved layout. `name`, `subject`
  (`student` | `staff`, default `student`, CHECK-constrained), `orientation`
  (`portrait` | `landscape`, default `portrait`, CHECK-constrained), `accent`
  and `background_color` (free-text colour strings), a nullable `background_key`
  (the object-store key of an uploaded background image), and `layout` — a JSONB
  array of fields. Timestamps `created_at` / `updated_at`.

`layout` is a JSON array of elements, each:

```json
{ "id": "…", "kind": "name", "x": 0.1, "y": 0.2, "w": 0.8,
  "fontSize": 24, "fontWeight": 700, "color": "#111", "align": "center", "text": "…" }
```

`kind` is one of `name`, `photo`, `id_number`, `class_or_role`, `valid_until`,
`blood_group`, `school_name`, `text`. `photo` marks where the person's photo goes;
`text` and `school_name` carry static copy. `x`, `y`, and `w` are **fractions of
the canvas in `[0,1]`**. The domain validates that the layout parses to an array
of at most 200 elements whose kinds are known and whose `x`/`y`/`w` are in
`[0,1]`; anything else is `ErrInvalidLayout`. An unknown or invalid subject or
orientation is `ErrInvalidTemplate`. The API carries `layout` through as raw JSON
— the designer owns its shape.

The listing is keyset-paginated, newest first (`created_at DESC, id DESC`), backed
by `id_card_templates_tenant_idx (tenant_id, created_at DESC, id DESC)` so the
cursor is a real keyset with no `Sort` node on the request path and no `OFFSET`.

## Background images

A background is uploaded straight to the object store, mirroring the certificate
designer flow. `PresignBackground` signs a one-shot PUT for an image of the
declared size (PNG/JPEG/WebP, up to 8 MiB) under the template's own key prefix;
`ConfirmBackground` heads the object, checks the key belongs to this template, and
records `background_key`, deleting the image it replaces (best-effort). On read the
service signs a short-lived inline GET as `background_url`; a template with no
background simply omits it.

## Endpoints

All require `academics:manage`, bearer auth.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v1/id-card-templates` | Templates, newest first. Keyset `cursor` + `has_more`. |
| `POST` | `/v1/id-card-templates` | Create a template. Body: `name?`, `subject?`, `orientation?`, `accent?`, `background_color?`, `layout?`. |
| `GET` | `/v1/id-card-templates/{id}` | One template, with a signed `background_url` when set. |
| `PUT` | `/v1/id-card-templates/{id}` | Update. Every field optional; an omitted field is left unchanged. |
| `DELETE` | `/v1/id-card-templates/{id}` | Delete a template. |
| `POST` | `/v1/id-card-templates/{id}/background/uploads` | Presign a background upload. Body: `content_type`, `bytes`. |
| `POST` | `/v1/id-card-templates/{id}/background` | Record an uploaded background. Body: `key`. |

## Errors

`ErrNotFound` → 404, `ErrInvalidTemplate` / `ErrInvalidLayout` / `ErrInvalidPage`
→ 422, mapped by `idCardError` in `internal/httpapi/idcard.go`.
