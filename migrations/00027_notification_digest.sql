-- +goose Up

-- Whether a notification has already been rolled into a digest email. NULL means
-- not yet; the daily digest includes the unread, not-yet-digested ones and stamps
-- them, so a retried digest job — River retries on failure — mails no one twice.
ALTER TABLE notifications ADD COLUMN digested_at timestamptz;

-- The digest sweep looks for unread, undigested notifications. A partial index
-- keeps that to the few rows that qualify, whatever the size of the table.
CREATE INDEX notifications_pending_digest_idx
    ON notifications (tenant_id, user_id)
    WHERE read_at IS NULL AND digested_at IS NULL;

-- A person's notification preferences. Absent means the defaults, so a row exists
-- only once someone changes something — and the digest defaults to on, because a
-- feature nobody was told about that mails them is the wrong surprise, but one
-- they can turn off is the right one.
CREATE TABLE notification_preferences (
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    email_digest boolean NOT NULL DEFAULT true,

    updated_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, user_id)
);

-- Row-level security. ENABLE alone exempts the table owner, the role the
-- application connects as; FORCE subjects the owner too.
ALTER TABLE notification_preferences ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification_preferences FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON notification_preferences
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE notification_preferences;
DROP INDEX notifications_pending_digest_idx;
ALTER TABLE notifications DROP COLUMN digested_at;
