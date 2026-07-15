-- +goose Up

-- The timetable: a weekly grid of periods per section. One row is one class slot —
-- a subject, at a time, on a weekday, with a teacher and a room. Time is stored as
-- a plain clock time (no date, no zone): a school day repeats every week, and the
-- weekday is the day_of_week column. The teacher is a name for now; a staff module
-- will make it a reference later without changing this shape.

CREATE TABLE timetable_periods (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    section_id   uuid NOT NULL REFERENCES sections (id) ON DELETE CASCADE,
    subject_id   uuid REFERENCES subjects (id) ON DELETE SET NULL,

    -- 0 = Sunday … 6 = Saturday. Sunday-first suits Bangladesh, where the school
    -- week runs Sunday to Thursday.
    day_of_week  smallint NOT NULL CHECK (day_of_week BETWEEN 0 AND 6),
    starts_at    time NOT NULL,
    ends_at      time NOT NULL CHECK (ends_at > starts_at),

    teacher_name text NOT NULL DEFAULT '',
    room         text NOT NULL DEFAULT '',

    created_at   timestamptz NOT NULL DEFAULT now()
);

-- One slot per section per weekday per start time: adding the same slot twice is a
-- conflict, not a duplicate row.
CREATE UNIQUE INDEX timetable_slot_key ON timetable_periods (tenant_id, section_id, day_of_week, starts_at);
-- A section's week, in grid order.
CREATE INDEX timetable_section_idx ON timetable_periods (tenant_id, section_id, day_of_week, starts_at);

ALTER TABLE timetable_periods ENABLE ROW LEVEL SECURITY;
ALTER TABLE timetable_periods FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON timetable_periods
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS timetable_periods;
