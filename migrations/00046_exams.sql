-- +goose Up

-- Exams and report cards. A grading scale is a table of bands (a percentage floor,
-- a letter, a grade point); a mark is one subject's score for one student in one
-- exam. The report card is never stored — it is computed from the marks against the
-- exam's scale, so a corrected mark is a corrected report card with no second write.
--
-- The tables are `exam_scales`/`exam_bands`, not `grading_scales`/`grading_bands`:
-- those names already belong to the LMS gradebook (00018), a different thing —
-- letter grades for a course, not a student's marks in a school exam.

CREATE TABLE exam_scales (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name       text NOT NULL,
    is_default boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- At most one default scale per workspace; the exam form reaches for it.
CREATE UNIQUE INDEX exam_scales_one_default ON exam_scales (tenant_id) WHERE is_default;
CREATE INDEX exam_scales_tenant_idx ON exam_scales (tenant_id, name);

CREATE TABLE exam_bands (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    scale_id    uuid NOT NULL REFERENCES exam_scales (id) ON DELETE CASCADE,

    letter      text NOT NULL,
    -- The inclusive percentage floor of the band. A score lands in the highest band
    -- whose floor it clears.
    min_percent numeric(5, 2) NOT NULL CHECK (min_percent >= 0 AND min_percent <= 100),
    gpa_point   numeric(4, 2) NOT NULL CHECK (gpa_point >= 0),
    is_pass     boolean NOT NULL DEFAULT true
);

CREATE UNIQUE INDEX exam_bands_scale_letter_key ON exam_bands (tenant_id, scale_id, letter);
CREATE INDEX exam_bands_scale_idx ON exam_bands (tenant_id, scale_id, min_percent DESC);

CREATE TABLE exams (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    name           text NOT NULL,
    term_id        uuid REFERENCES terms (id) ON DELETE SET NULL,
    grade_level_id uuid REFERENCES grade_levels (id) ON DELETE SET NULL,
    scale_id       uuid NOT NULL REFERENCES exam_scales (id) ON DELETE RESTRICT,

    held_on        date,
    status         text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published')),

    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX exams_tenant_created_idx ON exams (tenant_id, created_at DESC, id DESC);

CREATE TABLE exam_marks (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    exam_id    uuid NOT NULL REFERENCES exams (id) ON DELETE CASCADE,
    student_id uuid NOT NULL REFERENCES students (id) ON DELETE CASCADE,
    subject_id uuid NOT NULL REFERENCES subjects (id) ON DELETE CASCADE,

    full_marks numeric(6, 2) NOT NULL CHECK (full_marks > 0),
    obtained   numeric(6, 2) NOT NULL CHECK (obtained >= 0),

    marked_by  uuid REFERENCES users (id) ON DELETE SET NULL,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- One mark per student per subject per exam. The batch upsert conflicts on this.
CREATE UNIQUE INDEX exam_marks_unique ON exam_marks (tenant_id, exam_id, student_id, subject_id);
-- A whole exam's marks, and one student's report card within it.
CREATE INDEX exam_marks_exam_student_idx ON exam_marks (tenant_id, exam_id, student_id);

ALTER TABLE exam_scales ENABLE ROW LEVEL SECURITY;
ALTER TABLE exam_scales FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON exam_scales
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE exam_bands ENABLE ROW LEVEL SECURITY;
ALTER TABLE exam_bands FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON exam_bands
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE exams ENABLE ROW LEVEL SECURITY;
ALTER TABLE exams FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON exams
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE exam_marks ENABLE ROW LEVEL SECURITY;
ALTER TABLE exam_marks FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON exam_marks
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS exam_marks;
DROP TABLE IF EXISTS exams;
DROP TABLE IF EXISTS exam_bands;
DROP TABLE IF EXISTS exam_scales;
