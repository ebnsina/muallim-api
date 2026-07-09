-- +goose Up

-- Invitations are how a person joins a workspace they are not already in.
--
-- Without them, registration was the only door, and registration cannot open it:
-- a global account already exists, so the unique index on users.email rejects a
-- second one, and the users SELECT policy hides the first from a workspace the
-- person has no membership in. The user was stuck, and the 409 they received
-- leaked that their address existed somewhere on the platform.
--
-- Only a digest of the token is stored. An invitation link is a bearer
-- credential: whoever holds it becomes a member.
CREATE TABLE invitations (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    email      text        NOT NULL,
    role       text        NOT NULL CHECK (role IN ('owner', 'admin', 'instructor', 'student')),

    token_hash bytea       NOT NULL,
    invited_by uuid REFERENCES users (id) ON DELETE SET NULL,

    expires_at  timestamptz NOT NULL,
    accepted_at timestamptz,
    revoked_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX invitations_token_hash_key ON invitations (token_hash);

-- One outstanding invitation per address per workspace. A partial index, because
-- re-inviting someone after their first invitation expired or was accepted is
-- ordinary, not a conflict.
CREATE UNIQUE INDEX invitations_pending_email_key
    ON invitations (tenant_id, lower(email))
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

CREATE INDEX invitations_tenant_created_idx ON invitations (tenant_id, created_at DESC, id DESC);

ALTER TABLE invitations ENABLE ROW LEVEL SECURITY;
ALTER TABLE invitations FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON invitations
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- Accepting an invitation must be able to find the existing global account for
-- the invited address — otherwise the same chicken-and-egg that broke joining a
-- second workspace breaks accepting an invitation to one.
--
-- This widens visibility only to a workspace that holds a live, unaccepted
-- invitation for that exact address, which its own admin typed. It reveals
-- nothing the accept flow would not reveal anyway.
-- +goose StatementBegin
CREATE POLICY users_visible_to_inviting_tenant ON users FOR SELECT
    USING (
        app_current_tenant() IS NOT NULL
        AND EXISTS (
            SELECT 1 FROM invitations i
            WHERE lower(i.email) = lower(users.email)
              AND i.tenant_id = app_current_tenant()
              AND i.accepted_at IS NULL
              AND i.revoked_at IS NULL
              AND i.expires_at > now()
        )
    );
-- +goose StatementEnd

-- +goose Down
DROP POLICY users_visible_to_inviting_tenant ON users;
DROP TABLE invitations;
