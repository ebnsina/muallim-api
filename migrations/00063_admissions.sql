-- +goose Up

-- Admissions: application intake. A prospective student (or their guardian) applies;
-- the office reviews and accepts or rejects; an accepted applicant is later admitted,
-- which creates a student. This table is intake only — it never mints an account or a
-- student on its own. The admit step that produces a student is a cross-domain
-- orchestration wired by the coordinator, and it records the resulting student_id here.

CREATE TABLE admission_applications (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    applicant_name text NOT NULL,
    guardian_name  text NOT NULL DEFAULT '',
    guardian_phone text NOT NULL DEFAULT '',
    guardian_email text NOT NULL DEFAULT '',

    -- The class applied for, if named. Null until the office places the applicant.
    grade_level_id uuid REFERENCES grade_levels (id) ON DELETE SET NULL,
    dob            date,

    status         text NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending', 'accepted', 'rejected', 'admitted')),
    note           text NOT NULL DEFAULT '',

    -- The student created when the application is admitted; null until then.
    student_id     uuid REFERENCES students (id) ON DELETE SET NULL,

    submitted_at   timestamptz NOT NULL DEFAULT now(),
    decided_at     timestamptz,

    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- The intake board is keyset-paginated newest first; this index covers the filter and
-- the sort so EXPLAIN shows no Sort node on the request path.
CREATE INDEX admission_applications_tenant_idx
    ON admission_applications (tenant_id, submitted_at DESC, id DESC);
-- The status filter (the office works one queue at a time).
CREATE INDEX admission_applications_status_idx
    ON admission_applications (tenant_id, status);

ALTER TABLE admission_applications ENABLE ROW LEVEL SECURITY;
ALTER TABLE admission_applications FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON admission_applications
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS admission_applications;
