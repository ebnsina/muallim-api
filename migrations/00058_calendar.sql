-- +goose Up

-- Academic calendar: a school's holidays, exam dates, term boundaries, and events.
-- One entry per row. A single-day entry has no ends_on; a span carries one, and it
-- may never fall before the start. The kind is what the entry is; the client colours
-- and groups by it.

CREATE TABLE calendar_events (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    title       text NOT NULL,
    description text,

    kind        text NOT NULL DEFAULT 'event'
                CHECK (kind IN ('holiday', 'exam', 'event', 'term_start', 'term_end')),

    starts_on   date NOT NULL,
    ends_on     date CHECK (ends_on IS NULL OR ends_on >= starts_on),

    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- The calendar, newest first — the keyset shape.
CREATE INDEX calendar_events_tenant_starts_idx ON calendar_events (tenant_id, starts_on DESC, id DESC);
-- A date-window scan for a month or a term.
CREATE INDEX calendar_events_tenant_range_idx ON calendar_events (tenant_id, starts_on);

ALTER TABLE calendar_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE calendar_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON calendar_events
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS calendar_events;
