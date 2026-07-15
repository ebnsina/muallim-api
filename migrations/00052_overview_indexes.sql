-- +goose Up

-- The institution dashboard reads today's attendance across the whole workspace:
-- `WHERE tenant_id = $1 AND on_date = CURRENT_DATE`. The existing attendance
-- indexes lead with a student or a section, so that filter would fall to a scan of
-- the tenant's whole attendance history. This index leads with the date the
-- dashboard filters on, keeping that read an index scan.
CREATE INDEX attendance_tenant_date_idx ON attendance (tenant_id, on_date);

-- +goose Down
DROP INDEX IF EXISTS attendance_tenant_date_idx;
