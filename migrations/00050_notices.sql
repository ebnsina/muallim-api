-- +goose Up

-- Guardian notices: a school posts a message and it fans out to guardians. The
-- notice row is the record of what was said and to whom; the delivery is a job per
-- recipient, enqueued in the same transaction that posts the notice, so a notice
-- that commits is a notice that will be delivered, and one that rolls back queues
-- nothing. The recipient count is the fan-out width, recorded once at post time.
--
-- Delivery is by email today. A Bangladesh SMS driver will sit behind the same
-- interface next; the channel column is here so a notice remembers how it was sent.

CREATE TABLE notices (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    title           text NOT NULL,
    body            text NOT NULL,

    -- Who it went to: everyone's guardians, or the guardians of one class or section.
    audience        text NOT NULL
                    CHECK (audience IN ('all_guardians', 'class_guardians', 'section_guardians')),
    -- The class or section the audience narrows to, when it is not everyone.
    target_id       uuid,
    channel         text NOT NULL DEFAULT 'email' CHECK (channel IN ('email', 'sms', 'both')),

    recipient_count integer NOT NULL DEFAULT 0,
    posted_by       uuid REFERENCES users (id) ON DELETE SET NULL,

    created_at      timestamptz NOT NULL DEFAULT now()
);

-- The notice board, newest first.
CREATE INDEX notices_tenant_created_idx ON notices (tenant_id, created_at DESC, id DESC);

ALTER TABLE notices ENABLE ROW LEVEL SECURITY;
ALTER TABLE notices FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON notices
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS notices;
