-- +goose Up

-- A notification is a private message to one person: an answer to their question,
-- an announcement on a course they take, a grade on an essay they submitted.
--
-- Private like a note, so the tenant policy is the net and the service scopes
-- every read and write to user_id — one person never sees another's bell.
--
-- `kind` names the event so a client can group or route it; `link` is where the
-- item takes you. `read_at` null means unread; it is the only mutable column.
CREATE TABLE notifications (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    kind  text NOT NULL,
    title text NOT NULL,
    body  text NOT NULL DEFAULT '',
    link  text NOT NULL DEFAULT '',

    read_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- One person's notifications, newest first — the order a bell drops down, and the
-- keyset the list pages over.
CREATE INDEX notifications_recipient_idx
    ON notifications (tenant_id, user_id, created_at DESC, id);

-- The unread count for the header bell is a hot, tiny read; a partial index keeps
-- it to the unread rows only, so a person with ten thousand read notices still
-- counts their few unread ones in an index-only scan.
CREATE INDEX notifications_unread_idx
    ON notifications (tenant_id, user_id)
    WHERE read_at IS NULL;

-- Row-level security. ENABLE alone exempts the table owner, the role the
-- application connects as; FORCE subjects the owner too. Tenant isolation is the
-- net; that a person reads only their own bell is the service's job.
ALTER TABLE notifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE notifications FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON notifications
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE notifications;
