-- +goose Up

-- School transport. A route is a named line the institution runs, with a fare; a
-- vehicle is a bus on that route; an assignment puts one student on one route at a
-- stop. Money is bigint minor units + currency, defaulting to BDT poisha, never a
-- float — the same rule the fees and gateway ledgers follow.
--
-- A student rides one route at a time: the assignment is unique per student, and a
-- second placement replaces nothing silently — it is refused.

CREATE TABLE transport_routes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    name        text NOT NULL,
    description text,
    fare_amount bigint NOT NULL DEFAULT 0 CHECK (fare_amount >= 0),
    currency    char(3) NOT NULL DEFAULT 'BDT',

    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX transport_routes_tenant_idx ON transport_routes (tenant_id, created_at DESC, id DESC);

CREATE TABLE transport_vehicles (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    route_id        uuid NOT NULL REFERENCES transport_routes (id) ON DELETE CASCADE,
    registration_no text NOT NULL,
    capacity        int NOT NULL DEFAULT 0 CHECK (capacity >= 0),
    driver_name     text NOT NULL DEFAULT '',
    driver_phone    text,

    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- A route's fleet, newest first.
CREATE INDEX transport_vehicles_route_idx ON transport_vehicles (tenant_id, route_id, created_at DESC, id DESC);

CREATE TABLE transport_assignments (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    route_id    uuid NOT NULL REFERENCES transport_routes (id) ON DELETE CASCADE,
    student_id  uuid NOT NULL REFERENCES students (id) ON DELETE CASCADE,
    stop_name   text NOT NULL DEFAULT '',
    assigned_at timestamptz NOT NULL DEFAULT now(),

    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- One route per student: the assign conflicts on this so a second placement is
-- refused rather than duplicated.
CREATE UNIQUE INDEX transport_assignments_student_key ON transport_assignments (tenant_id, student_id);
-- The workspace-wide list, newest first, and the two filter shapes.
CREATE INDEX transport_assignments_tenant_idx ON transport_assignments (tenant_id, created_at DESC, id DESC);
CREATE INDEX transport_assignments_route_idx ON transport_assignments (tenant_id, route_id, created_at DESC, id DESC);

ALTER TABLE transport_routes ENABLE ROW LEVEL SECURITY;
ALTER TABLE transport_routes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON transport_routes
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE transport_vehicles ENABLE ROW LEVEL SECURITY;
ALTER TABLE transport_vehicles FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON transport_vehicles
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE transport_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE transport_assignments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON transport_assignments
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS transport_assignments;
DROP TABLE IF EXISTS transport_vehicles;
DROP TABLE IF EXISTS transport_routes;
