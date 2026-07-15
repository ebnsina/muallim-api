-- +goose Up

-- Subjects: the catalog of what an institution teaches (Bangla, Mathematics,
-- Quran, Fiqh). Workspace-level; exams and report cards mark against them later.
-- A code is optional but unique when present — schools address a subject by code.

CREATE TABLE subjects (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    name text NOT NULL,
    code text NOT NULL DEFAULT '',

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX subjects_tenant_name_key ON subjects (tenant_id, lower(name));
-- Codes are unique only when set; a blank code is not a collision.
CREATE UNIQUE INDEX subjects_tenant_code_key ON subjects (tenant_id, lower(code)) WHERE code <> '';
CREATE INDEX subjects_tenant_idx ON subjects (tenant_id, name, id);

ALTER TABLE subjects ENABLE ROW LEVEL SECURITY;
ALTER TABLE subjects FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON subjects
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS subjects;
