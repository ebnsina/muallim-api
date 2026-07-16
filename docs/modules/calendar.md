# Academic calendar

A school's calendar: its holidays, exam dates, term boundaries, and events. A
modular-monolith domain like the rest — it knows nothing about HTTP, and is
tenant-scoped with RLS behind the one table it owns.

## Model

- **`calendar_events`** — one entry on the calendar. A `title`, an optional
  `description`, a `kind` (`holiday`, `exam`, `event`, `term_start`, `term_end`),
  a required `starts_on` date, and an optional `ends_on` date. A single-day entry
  has no `ends_on`; a span carries one, and a `CHECK` keeps `ends_on >= starts_on`
  so an event can never end before it starts.

The listing is keyset-paginated, newest first (`starts_on DESC, id DESC`) over the
`(tenant_id, starts_on DESC, id DESC)` index; the from/to date window rides
`(tenant_id, starts_on)`. No `Sort` node on the request path, no `OFFSET`.

## Endpoints

All under `academics:manage`, admin-only, bearer auth.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v1/calendar/events` | The calendar, newest first. Filter by `kind` and by a start-date window (`from`, `to`). Keyset `cursor` + `has_more`. |
| `POST` | `/v1/calendar/events` | Add an event. Body: `title`, `description?`, `kind?`, `starts_on`, `ends_on?`. |
| `PATCH` / `PUT` | `/v1/calendar/events/{id}` | Edit an event. A nil field is left unchanged. |
| `DELETE` | `/v1/calendar/events/{id}` | Remove an event. |

## Errors

| Sentinel | Status | When |
| --- | --- | --- |
| `ErrNotFound` | 404 | No such event in this workspace. |
| `ErrInvalidEvent` | 422 | Empty title, unknown kind, or `ends_on` before `starts_on`. |
| `ErrInvalidPage` | 422 | The page cursor did not decode. |
