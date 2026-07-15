-- +goose Up

-- Attendance: the daily register. One row per student per day — re-marking a day
-- updates it rather than stacking a second row, which the unique index enforces and
-- the upsert relies on. The section is the context it was taken in; the marker is
-- who took it. Attendance rates are read from these rows, never stored, so a
-- correction to a day is a correction to every rate that day feeds.

CREATE TABLE attendance (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    student_id uuid NOT NULL REFERENCES students (id) ON DELETE CASCADE,
    section_id uuid REFERENCES sections (id) ON DELETE SET NULL,

    on_date date NOT NULL,
    status  text NOT NULL CHECK (status IN ('present', 'absent', 'late', 'excused')),

    marked_by uuid REFERENCES users (id) ON DELETE SET NULL,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- One mark per student per day. The batch upsert conflicts on exactly this.
CREATE UNIQUE INDEX attendance_student_date_key ON attendance (tenant_id, student_id, on_date);
-- The register for a section on a day, and a student's own history over a range.
CREATE INDEX attendance_section_date_idx ON attendance (tenant_id, section_id, on_date);
CREATE INDEX attendance_student_date_idx ON attendance (tenant_id, student_id, on_date);

ALTER TABLE attendance ENABLE ROW LEVEL SECURITY;
ALTER TABLE attendance FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON attendance
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS attendance;
