-- +goose Up

-- A password is global; a session is tenant-scoped. Resetting a password revoked
-- only the sessions in the workspace the reset was requested from, so a person
-- who belongs to two workspaces changed their password and left the other one
-- logged in — on whatever device had it, including the one they were trying to
-- lock out.
--
-- The revocation therefore has to reach every workspace, which means running
-- unbound. It cannot be widened within a bound tenant instead: an UPDATE reads
-- the rows it changes, so granting one workspace the right to revoke another's
-- sessions would first grant it the right to read them, token digests and all.
--
-- Only WithoutTenant reaches this, and every one of its call sites is a place to
-- look twice.
CREATE POLICY sessions_maintenance_unbound ON sessions FOR SELECT
    USING (app_current_tenant() IS NULL);

CREATE POLICY sessions_revoke_unbound ON sessions FOR UPDATE
    USING (app_current_tenant() IS NULL)
    WITH CHECK (app_current_tenant() IS NULL);

-- +goose Down
DROP POLICY sessions_revoke_unbound ON sessions;
DROP POLICY sessions_maintenance_unbound ON sessions;
