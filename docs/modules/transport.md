# Transport

School transport: the routes an institution runs, the vehicles on them, and the
students assigned to a route. Tenant-scoped and RLS-backed like every domain; money
is `bigint` minor units + `currency char(3)`, defaulting to BDT poisha, never a
float.

## Model

- **Route** (`transport_routes`) — a named line with a fare (`fare_amount`,
  `currency`). Optional `description`.
- **Vehicle** (`transport_vehicles`) — a bus on a route: `registration_no`,
  `capacity`, `driver_name`, optional `driver_phone`. Deleted with its route.
- **Assignment** (`transport_assignments`) — one student on one route at a
  `stop_name`, with an `assigned_at`. **Unique per `(tenant_id, student_id)`**: a
  student rides one route at a time, so a second placement is refused rather than
  silently replaced. Deleted with its route or its student.

## Endpoints

All require `academics:manage`.

| Method | Path | Purpose |
| ------ | ---- | ------- |
| GET    | `/v1/transport/routes` | List routes, newest first (keyset: `limit`, `cursor` → `next_cursor` + `has_more`). |
| POST   | `/v1/transport/routes` | Create a route. |
| GET    | `/v1/transport/routes/{id}/vehicles` | A route's fleet, newest first. |
| POST   | `/v1/transport/routes/{id}/vehicles` | Add a vehicle to the route. |
| GET    | `/v1/transport/assignments` | List assignments, newest first (keyset). Filter by `route_id` and/or `student_id`. |
| POST   | `/v1/transport/assignments` | Assign a student to a route. |
| DELETE | `/v1/transport/assignments/{id}` | Unassign a student. |

## Errors

- `404` — unknown route, vehicle-on-missing-route, assign of an unknown student
  (FK), or unassign of a missing assignment.
- `409` — the student already rides a route (`ErrAlreadyAssigned`).
- `422` — an invalid route, vehicle, assignment, or page cursor.
