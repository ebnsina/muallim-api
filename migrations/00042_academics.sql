-- +goose Up

-- The academic layer: the spine any institution — school, college, madrasa, or
-- coaching center — organises itself around. A calendar (years and their terms) and
-- a structure (classes and their sections). Students, attendance, exams and fees
-- hang off these in later migrations.
--
-- Every table is tenant-scoped with an RLS policy behind the application filter, as
-- the rest of the schema is: RLS is the net, not the primary control.

-- What kind of institution this workspace is. It changes vocabulary and defaults —
-- a madrasa's grading ladder, a coaching centre's batches — not the tables here.
ALTER TABLE tenants
    ADD COLUMN institution_type text NOT NULL DEFAULT 'school'
        CHECK (institution_type IN ('school', 'college', 'madrasa', 'coaching'));

-- An academic year: the calendar everything is scheduled within. At most one is the
-- current one, enforced by a partial unique index rather than trusted to the caller.
CREATE TABLE academic_years (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    name       text NOT NULL,
    starts_on  date NOT NULL,
    ends_on    date NOT NULL,
    is_current boolean NOT NULL DEFAULT false,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    CHECK (ends_on > starts_on)
);

CREATE UNIQUE INDEX academic_years_tenant_name_key ON academic_years (tenant_id, lower(name));
-- One current year per workspace, or none. The database refuses a second.
CREATE UNIQUE INDEX academic_years_one_current ON academic_years (tenant_id) WHERE is_current;

-- A term (semester, quarter) within a year, in the author's order.
CREATE TABLE terms (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    academic_year_id uuid NOT NULL REFERENCES academic_years (id) ON DELETE CASCADE,

    name      text NOT NULL,
    starts_on date NOT NULL,
    ends_on   date NOT NULL,
    position  int  NOT NULL DEFAULT 0,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    CHECK (ends_on > starts_on)
);

CREATE INDEX terms_year_idx ON terms (tenant_id, academic_year_id, position, id);

-- A class / grade level (Class 6, Dakhil 1st Year, Batch A). `rank` orders them by
-- seniority so a listing reads junior to senior without alphabetising "Class 10"
-- before "Class 2".
CREATE TABLE grade_levels (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    name text NOT NULL,
    rank int  NOT NULL DEFAULT 0,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX grade_levels_tenant_name_key ON grade_levels (tenant_id, lower(name));
CREATE INDEX grade_levels_tenant_rank_idx ON grade_levels (tenant_id, rank, id);

-- A section within a class (6-A, 6-B). Capacity is a soft cap: zero means unset.
CREATE TABLE sections (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    grade_level_id uuid NOT NULL REFERENCES grade_levels (id) ON DELETE CASCADE,

    name     text NOT NULL,
    capacity int  NOT NULL DEFAULT 0 CHECK (capacity >= 0),

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX sections_grade_name_key ON sections (tenant_id, grade_level_id, lower(name));
CREATE INDEX sections_tenant_grade_idx ON sections (tenant_id, grade_level_id, name);

-- RLS on every table: the net behind the application's tenant_id filter.
ALTER TABLE academic_years ENABLE ROW LEVEL SECURITY;
ALTER TABLE academic_years FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON academic_years
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE terms ENABLE ROW LEVEL SECURITY;
ALTER TABLE terms FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON terms
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE grade_levels ENABLE ROW LEVEL SECURITY;
ALTER TABLE grade_levels FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON grade_levels
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE sections ENABLE ROW LEVEL SECURITY;
ALTER TABLE sections FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON sections
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS sections;
DROP TABLE IF EXISTS grade_levels;
DROP TABLE IF EXISTS terms;
DROP TABLE IF EXISTS academic_years;
ALTER TABLE tenants DROP COLUMN IF EXISTS institution_type;
