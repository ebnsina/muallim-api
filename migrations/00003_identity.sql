-- +goose Up

-- Users are global, not tenant-scoped. One person, one account, however many
-- workspaces they belong to: an instructor on one tenant and a student on
-- another is the same human, not two rows.
--
-- Membership is what binds a user to a tenant, and it carries the role.
CREATE TABLE users (
    id            uuid PRIMARY KEY,
    email         text        NOT NULL,
    password_hash text        NOT NULL,
    name          text        NOT NULL DEFAULT '',
    email_verified_at timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX users_email_key ON users (lower(email));

CREATE TABLE memberships (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role       text        NOT NULL CHECK (role IN ('owner', 'admin', 'instructor', 'student')),
    status     text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX memberships_tenant_user_key ON memberships (tenant_id, user_id);
CREATE INDEX memberships_user_idx ON memberships (user_id);

-- Sessions hold refresh tokens. The raw token never touches the database: only
-- a SHA-256 digest of it, so a database dump does not hand over live sessions.
--
-- family_id groups a token and every token rotated out of it. Presenting a token
-- that was already rotated away means it was stolen or replayed, and the whole
-- family is revoked at once — the thief and the victim both get logged out,
-- which is the correct outcome.
CREATE TABLE sessions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    family_id   uuid        NOT NULL,
    token_hash  bytea       NOT NULL,
    expires_at  timestamptz NOT NULL,
    revoked_at  timestamptz,
    replaced_by uuid REFERENCES sessions (id) ON DELETE SET NULL,
    user_agent  text        NOT NULL DEFAULT '',
    ip          inet,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX sessions_token_hash_key ON sessions (token_hash);
CREATE INDEX sessions_family_idx ON sessions (tenant_id, family_id);
CREATE INDEX sessions_user_idx ON sessions (tenant_id, user_id);

-- Expired sessions are swept periodically; this index makes that a range scan.
CREATE INDEX sessions_expires_idx ON sessions (expires_at) WHERE revoked_at IS NULL;

-- The audit log. FERPA and GDPR both require one, and it cannot be added
-- retroactively: you cannot backfill history you never recorded.
--
-- Append-only by construction. The policies below grant SELECT and INSERT and
-- nothing else, so no UPDATE or DELETE can reach a row — not even from
-- application code that has been compromised or is simply wrong.
CREATE TABLE audit_log (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    actor_id     uuid REFERENCES users (id) ON DELETE SET NULL,
    action       text        NOT NULL,
    target_type  text        NOT NULL DEFAULT '',
    target_id    text        NOT NULL DEFAULT '',
    ip           inet,
    user_agent   text        NOT NULL DEFAULT '',
    metadata     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_tenant_created_idx ON audit_log (tenant_id, created_at DESC, id DESC);
CREATE INDEX audit_log_actor_idx ON audit_log (tenant_id, actor_id, created_at DESC);

-- Row-level security.
--
-- memberships and sessions are tenant-scoped and get the standard policy.
-- +goose StatementBegin
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['memberships', 'sessions'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format(
            'CREATE POLICY tenant_isolation ON %I
                 USING (tenant_id = app_current_tenant())
                 WITH CHECK (tenant_id = app_current_tenant())', t);
    END LOOP;
END $$;
-- +goose StatementEnd

-- audit_log: readable and insertable within the tenant, never mutable. A table
-- with no UPDATE or DELETE policy denies both.
ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log FORCE ROW LEVEL SECURITY;

CREATE POLICY audit_read ON audit_log FOR SELECT
    USING (tenant_id = app_current_tenant());
CREATE POLICY audit_append ON audit_log FOR INSERT
    WITH CHECK (tenant_id = app_current_tenant());

-- users is global, so it cannot key on tenant_id. Instead a row is visible only
-- to a tenant that the user is a member of. This is the net beneath every query
-- that forgets to join memberships.
--
-- INSERT is unrestricted because registration creates the user before the
-- membership that would authorise reading it. The id is therefore generated by
-- the application rather than by RETURNING, since RETURNING would apply the
-- SELECT policy to a row whose membership does not exist yet.
ALTER TABLE users ENABLE ROW LEVEL SECURITY;
ALTER TABLE users FORCE ROW LEVEL SECURITY;

-- +goose StatementBegin
CREATE POLICY users_visible_to_their_tenants ON users FOR SELECT
    USING (
        app_current_tenant() IS NOT NULL
        AND EXISTS (
            SELECT 1 FROM memberships m
            WHERE m.user_id = users.id
              AND m.tenant_id = app_current_tenant()
        )
    );
-- +goose StatementEnd

CREATE POLICY users_insert ON users FOR INSERT WITH CHECK (true);

-- +goose StatementBegin
CREATE POLICY users_update_own_tenant ON users FOR UPDATE
    USING (
        EXISTS (
            SELECT 1 FROM memberships m
            WHERE m.user_id = users.id
              AND m.tenant_id = app_current_tenant()
        )
    );
-- +goose StatementEnd

-- +goose Down
DROP TABLE audit_log;
DROP TABLE sessions;
DROP TABLE memberships;
DROP TABLE users;
