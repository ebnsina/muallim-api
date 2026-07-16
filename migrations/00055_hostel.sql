-- +goose Up

-- Hostel (boarding) management: the buildings an institution runs, the rooms inside
-- them, and which student sleeps in which bed. A room has a capacity, and its
-- `occupied` count is kept in step with active allocations in the same statement that
-- allocates or vacates — so the count can never disagree with the rows it summarises.
--
-- An allocation is active until the student is vacated; a student may hold exactly one
-- active allocation at a time. Vacating keeps the row for the boarding history rather
-- than deleting it.

CREATE TABLE hostel_buildings (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    name         text NOT NULL,
    warden_name  text,
    warden_phone text,

    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- The building list, by name.
CREATE INDEX hostel_buildings_tenant_idx ON hostel_buildings (tenant_id, name, id);

CREATE TABLE hostel_rooms (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    building_id uuid NOT NULL REFERENCES hostel_buildings (id) ON DELETE CASCADE,
    room_no     text NOT NULL,
    capacity    int NOT NULL DEFAULT 1 CHECK (capacity >= 0),
    occupied    int NOT NULL DEFAULT 0 CHECK (occupied >= 0 AND occupied <= capacity),

    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- The rooms in a building, by room number.
CREATE INDEX hostel_rooms_building_idx ON hostel_rooms (tenant_id, building_id, room_no, id);

CREATE TABLE hostel_allocations (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    room_id      uuid NOT NULL REFERENCES hostel_rooms (id) ON DELETE CASCADE,
    student_id   uuid NOT NULL REFERENCES students (id) ON DELETE CASCADE,

    allocated_at timestamptz NOT NULL DEFAULT now(),
    vacated_at   timestamptz,
    status       text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'vacated')),

    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- One active bed per student: a student cannot be boarded in two rooms at once.
-- A vacated allocation is history and does not block a fresh one.
CREATE UNIQUE INDEX hostel_allocations_active_student_key
    ON hostel_allocations (tenant_id, student_id) WHERE status = 'active';
-- The allocation list, newest first, keyset-paginated.
CREATE INDEX hostel_allocations_tenant_idx ON hostel_allocations (tenant_id, allocated_at DESC, id DESC);
-- Narrowed to a room, and to a student, each with the same sort so no Sort node lands.
CREATE INDEX hostel_allocations_room_idx ON hostel_allocations (tenant_id, room_id, allocated_at DESC, id DESC);
CREATE INDEX hostel_allocations_student_idx ON hostel_allocations (tenant_id, student_id, allocated_at DESC, id DESC);

ALTER TABLE hostel_buildings ENABLE ROW LEVEL SECURITY;
ALTER TABLE hostel_buildings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON hostel_buildings
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE hostel_rooms ENABLE ROW LEVEL SECURITY;
ALTER TABLE hostel_rooms FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON hostel_rooms
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE hostel_allocations ENABLE ROW LEVEL SECURITY;
ALTER TABLE hostel_allocations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON hostel_allocations
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS hostel_allocations;
DROP TABLE IF EXISTS hostel_rooms;
DROP TABLE IF EXISTS hostel_buildings;
