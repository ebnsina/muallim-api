# Hostel

Boarding (hostel) management: the buildings an institution runs, the rooms inside
them, and which student is allocated a bed. A modular-monolith domain like the rest —
it knows nothing about HTTP, references students by id, and is tenant-scoped with RLS
behind every table.

## Model

- **`hostel_buildings`** — a boarding house. `name`, optional `warden_name` and
  `warden_phone`.
- **`hostel_rooms`** — a room in a building. `building_id`, `room_no`, `capacity`,
  and a live `occupied` count. A check constraint keeps `0 <= occupied <= capacity`.
- **`hostel_allocations`** — one student's bed in one room. `room_id`, `student_id`,
  `allocated_at`, nullable `vacated_at`, and a `status` of `active` or `vacated`. A
  unique partial index over `(tenant_id, student_id) WHERE status = 'active'` gives a
  student at most one active bed.

Allocating a room claims a bed (`occupied + 1`, guarded `occupied < capacity` in the
same statement, so a full room takes nobody — `ErrRoomFull`) and inserts the active
allocation (the one-active-bed index refuses a student already boarded —
`ErrAlreadyAllocated`). Vacating frees the bed (`GREATEST(occupied - 1, 0)`), guarded
by `status = 'active'` so a repeat vacate is a no-op, and keeps the row as boarding
history rather than deleting it. The `occupied` roll-up is recomputed in the same
transaction as the allocation that changes it — never on read, never in a trigger.

Buildings and allocations are keyset-paginated (buildings by `name`, allocations
newest first by `allocated_at DESC, id DESC`), and each filter shape has an index
that covers its sort — no `Sort` node on the request path, no `OFFSET`. A building's
rooms are a bounded list, by room number.

## Endpoints

All under `academics:manage`, admin-only, bearer auth.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v1/hostel/buildings` | Buildings, by name. Keyset `cursor` + `has_more`. |
| `POST` | `/v1/hostel/buildings` | Register a building. Body: `name`, `warden_name?`, `warden_phone?`. |
| `GET` | `/v1/hostel/buildings/{id}/rooms` | A building's rooms, by room number. |
| `POST` | `/v1/hostel/buildings/{id}/rooms` | Add a room. Body: `room_no`, `capacity?` (defaults to 1). |
| `GET` | `/v1/hostel/allocations` | Allocations, newest first. Filter by `room_id`, `student_id` and/or `status`. Keyset `cursor` + `has_more`. |
| `POST` | `/v1/hostel/allocations` | Allocate a room. Body: `room_id`, `student_id`. |
| `DELETE` | `/v1/hostel/allocations/{id}` | Vacate an allocation; frees the bed, keeps the history. Idempotent. |

## Errors

| Sentinel | Status | When |
| --- | --- | --- |
| `ErrNotFound` | 404 | No such building, room, or allocation in this workspace. |
| `ErrRoomFull` | 409 | Every bed in the room is already taken. |
| `ErrAlreadyAllocated` | 409 | The student already holds an active bed. |
| `ErrInvalidBuilding` | 422 | Missing name. |
| `ErrInvalidRoom` | 422 | Missing room number or building, or a negative capacity. |
| `ErrInvalidAllocation` | 422 | Missing room or student. |
| `ErrInvalidPage` | 422 | The page cursor did not decode. |
