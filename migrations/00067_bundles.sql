-- +goose Up

-- Course bundles. A bundle groups several courses under one name and one price,
-- so a workspace can sell them together. Money is bigint minor units + currency,
-- defaulting to BDT poisha, never a float — a bundle with price 0 is free, the way
-- a course with no `course_prices` row is free.
--
-- This is a self-contained domain: it references courses (id) by foreign key and
-- knows nothing of catalog, commerce, or enrol. Granting a bundle enrols the
-- learner in each of its courses; that step is wired by the coordinator, which
-- reads a bundle's course ids from here and calls enrol itself.

CREATE TABLE bundles (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    slug         text        NOT NULL,
    name         text        NOT NULL,
    description  text        NOT NULL DEFAULT '',
    price_amount bigint      NOT NULL DEFAULT 0 CHECK (price_amount >= 0),
    currency     char(3)     NOT NULL DEFAULT 'BDT',

    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- Slugs are unique per tenant, not globally: two schools may both sell a "starter-pack".
CREATE UNIQUE INDEX bundles_tenant_slug_key ON bundles (tenant_id, lower(slug));

-- The bundle list keyset-paginates by (created_at, id) descending. This index covers
-- the filter and the sort, so the plan is an index scan with no Sort node, and page
-- 500 costs what page 1 costs.
CREATE INDEX bundles_tenant_created_idx
    ON bundles (tenant_id, created_at DESC, id DESC);

CREATE TABLE bundle_courses (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    bundle_id  uuid        NOT NULL REFERENCES bundles (id) ON DELETE CASCADE,
    course_id  uuid        NOT NULL REFERENCES courses (id) ON DELETE CASCADE,
    position   integer     NOT NULL DEFAULT 0,

    created_at timestamptz NOT NULL DEFAULT now()
);

-- A course appears in a bundle at most once.
CREATE UNIQUE INDEX bundle_courses_unique_key
    ON bundle_courses (tenant_id, bundle_id, course_id);

-- Loading a bundle's ordered courses (and the batched load of several bundles'
-- courses via `= ANY`) is an index scan that returns rows already ordered.
CREATE INDEX bundle_courses_bundle_position_idx
    ON bundle_courses (tenant_id, bundle_id, position, id);

ALTER TABLE bundles ENABLE ROW LEVEL SECURITY;
ALTER TABLE bundles FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON bundles
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE bundle_courses ENABLE ROW LEVEL SECURITY;
ALTER TABLE bundle_courses FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON bundle_courses
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS bundle_courses;
DROP TABLE IF EXISTS bundles;
