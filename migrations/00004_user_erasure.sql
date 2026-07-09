-- +goose Up

-- Users had no DELETE policy, and a table with FORCE ROW LEVEL SECURITY denies
-- every command it has no policy for. The row was therefore unerasable, which
-- quietly made GDPR's right to erasure impossible to honour.
--
-- The policy grants deletion of a user who is a member of the current tenant.
--
-- It deliberately does NOT try to check that this is the user's *only* tenant.
-- That check cannot be written here: a policy's subquery reads `memberships`
-- through `memberships`' own row-level security, so it can only ever see rows
-- belonging to the current tenant. A `NOT EXISTS (... WHERE tenant_id <>
-- app_current_tenant())` clause is therefore vacuously true and grants exactly
-- what it appears to forbid. Enforcing it here would be worse than not enforcing
-- it, because it would look enforced.
--
-- A user is global — one account across every workspace they belong to — so
-- erasing the row erases them everywhere. The sole-membership guard belongs in
-- application code, which can read memberships across tenants via
-- database.WithoutTenant. Removing a user from one workspace means deleting
-- their membership, not their account.
-- +goose StatementBegin
CREATE POLICY users_erase_own_member ON users FOR DELETE
    USING (
        app_current_tenant() IS NOT NULL
        AND EXISTS (
            SELECT 1 FROM memberships m
            WHERE m.user_id = users.id
              AND m.tenant_id = app_current_tenant()
        )
    );
-- +goose StatementEnd

-- +goose Down
DROP POLICY users_erase_own_member ON users;
