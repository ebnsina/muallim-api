-- +goose Up

-- A credential token proves control of an email address. It either confirms the
-- address, or it authorises setting a new password without knowing the old one.
--
-- Both are bearer credentials, so only the SHA-256 digest is stored — the same
-- treatment sessions and invitations already get. A database dump must not be a
-- set of live password resets.
--
-- The row is tenant-scoped even though a user and their password are global. A
-- reset is requested from a workspace, by an address that must already be a
-- member of it, and that is the property RLS enforces here: nobody can mint a
-- reset token for a stranger by pointing at someone else's workspace.
CREATE TABLE user_tokens (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    purpose   text NOT NULL CHECK (purpose IN ('email_verification', 'password_reset')),

    token_hash bytea NOT NULL,

    expires_at  timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX user_tokens_token_hash_key ON user_tokens (token_hash);

-- Requesting a fresh token supersedes the outstanding ones for that purpose, in
-- one UPDATE served by this index. Otherwise every reset email a user ever
-- requested stays live until it expires, and the oldest link in their inbox works
-- as well as the newest.
CREATE INDEX user_tokens_outstanding_idx
    ON user_tokens (tenant_id, user_id, purpose)
    WHERE consumed_at IS NULL;

ALTER TABLE user_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_tokens FORCE ROW LEVEL SECURITY;

-- One policy covers SELECT, INSERT, and UPDATE. A table with FORCE ROW LEVEL
-- SECURITY denies every command it has no policy for, silently, by matching zero
-- rows — so a missing UPDATE policy would not fail the consume, it would just
-- never consume anything, and the token would stay reusable.
CREATE POLICY tenant_isolation ON user_tokens
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE user_tokens;
