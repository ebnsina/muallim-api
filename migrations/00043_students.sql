-- +goose Up

-- The people the academic structure is for: students, their guardians, and the link
-- between them. A student belongs to a class and a section (both optional until they
-- are placed), and may or may not hold a login account — a young child does not, an
-- older learner might, so user_id is nullable and never assumed.

CREATE TABLE students (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    admission_no text NOT NULL,
    full_name    text NOT NULL,

    grade_level_id uuid REFERENCES grade_levels (id) ON DELETE SET NULL,
    section_id     uuid REFERENCES sections (id) ON DELETE SET NULL,
    roll           int NOT NULL DEFAULT 0,

    status text NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'inactive', 'graduated', 'transferred')),

    -- The global user account, when the student has one. A membership makes them a
    -- login; its absence is a child on a roll, not an error.
    user_id uuid REFERENCES users (id) ON DELETE SET NULL,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Admission numbers are the school's own primary key for a student; unique per
-- workspace, case-insensitively.
CREATE UNIQUE INDEX students_tenant_admission_key ON students (tenant_id, lower(admission_no));

-- The roster is keyset-paginated by (full_name, id). A school has thousands of
-- students, so this is a cursor, not a bounded list — and each query shape gets an
-- index that covers its filter and its sort, or EXPLAIN shows a Sort node.
CREATE INDEX students_tenant_name_idx ON students (tenant_id, full_name, id);
CREATE INDEX students_grade_name_idx ON students (tenant_id, grade_level_id, full_name, id);
CREATE INDEX students_section_name_idx ON students (tenant_id, section_id, full_name, id);

CREATE TABLE guardians (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    full_name text NOT NULL,
    phone     text NOT NULL DEFAULT '',
    email     text NOT NULL DEFAULT '',
    relation  text NOT NULL DEFAULT '',

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX guardians_tenant_idx ON guardians (tenant_id, full_name, id);

-- A student's guardians. One is the primary contact, whom notices reach first.
CREATE TABLE student_guardians (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    student_id  uuid NOT NULL REFERENCES students (id) ON DELETE CASCADE,
    guardian_id uuid NOT NULL REFERENCES guardians (id) ON DELETE CASCADE,

    is_primary boolean NOT NULL DEFAULT false,

    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX student_guardians_pair_key ON student_guardians (tenant_id, student_id, guardian_id);
CREATE INDEX student_guardians_student_idx ON student_guardians (tenant_id, student_id);

ALTER TABLE students ENABLE ROW LEVEL SECURITY;
ALTER TABLE students FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON students
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE guardians ENABLE ROW LEVEL SECURITY;
ALTER TABLE guardians FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON guardians
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE student_guardians ENABLE ROW LEVEL SECURITY;
ALTER TABLE student_guardians FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON student_guardians
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS student_guardians;
DROP TABLE IF EXISTS guardians;
DROP TABLE IF EXISTS students;
