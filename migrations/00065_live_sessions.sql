-- +goose Up

-- Live class sessions. An instructor schedules a session on a course and pastes a
-- meeting URL (Zoom/Meet/Jitsi/anything); enrolled learners see it and join. There
-- is no video infrastructure here — this is scheduling plus a stored link, gated by
-- the course's own access model. Who may read a session is decided in the HTTP
-- layer from enrolment, exactly as a lesson is.

CREATE TABLE live_sessions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    course_id    uuid NOT NULL REFERENCES courses (id) ON DELETE CASCADE,

    title        text NOT NULL,
    description  text NOT NULL DEFAULT '',
    -- The meeting link the instructor pastes. Nullable: a session may be scheduled
    -- before its link exists.
    join_url     text,

    starts_at    timestamptz NOT NULL,
    ends_at      timestamptz CHECK (ends_at IS NULL OR ends_at >= starts_at),

    -- Who is hosting. Kept as a row even if the account is later removed.
    host_user_id uuid REFERENCES users (id) ON DELETE SET NULL,

    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- A course's sessions, soonest/newest first. Covers the filter (tenant, course)
-- and the sort (starts_at DESC, id DESC), so the keyset never leaves a Sort node.
CREATE INDEX live_sessions_course_idx
    ON live_sessions (tenant_id, course_id, starts_at DESC, id DESC);

ALTER TABLE live_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE live_sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON live_sessions
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS live_sessions;
