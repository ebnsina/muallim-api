-- +goose Up

-- Deleting a workspace cascades its memberships away, and so does removing its
-- last member. Either leaves a user with no membership anywhere: an orphan.
--
-- An orphan was unerasable. The users DELETE policy from 00004 requires a
-- membership in the current tenant, and an orphan has none — in any tenant. So
-- GDPR's right to erasure could not be honoured for exactly the people most
-- likely to ask for it: the ones who have left.
--
-- The obvious fix was thought to need a role with BYPASSRLS, which the migration
-- role deliberately is not. It does not. It needs the orphan check to be *true*,
-- and 00004 explains why it currently cannot be: a policy's subquery reads
-- memberships through memberships' own row-level security, so under an unbound
-- session it sees nothing and `NOT EXISTS` is vacuously true — granting exactly
-- what it appears to forbid.
--
-- Make memberships visible to an unbound session, and the subquery becomes real.

-- An unbound session is not a request. database.WithTenant refuses uuid.Nil, and
-- every tenant-scoped route binds a tenant before a handler runs; only
-- WithoutTenant reaches this, and every one of its call sites is a place to look
-- twice. Widening SELECT here is what lets the policies below check what they
-- claim to check, rather than looking as though they do.
CREATE POLICY memberships_visible_unbound ON memberships FOR SELECT
    USING (app_current_tenant() IS NULL);

-- With memberships visible, `NOT EXISTS` means what it says. An orphan is
-- visible to an unbound session, and nobody else; a user who still belongs to
-- any workspace is not, whoever is asking.
--
-- This is the control that matters. Postgres applies SELECT policies to the rows
-- an UPDATE or DELETE has to read, so a member is not merely undeletable — they
-- are unseeable, and the DELETE matches nothing. It re-scans on its own
-- statement snapshot, so a user who joins a workspace between the select and the
-- delete is spared rather than raced away.
-- +goose StatementBegin
CREATE POLICY users_orphan_visible ON users FOR SELECT
    USING (
        app_current_tenant() IS NULL
        AND NOT EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = users.id)
    );
-- +goose StatementEnd

-- The same predicate on DELETE is redundant today, and deliberately kept.
-- Deletion is currently bounded by what SELECT reveals; the day somebody adds a
-- broader SELECT policy here — support tooling, an admin console — that bound
-- disappears silently, and this is what stops erasure from widening with it.
-- +goose StatementBegin
CREATE POLICY users_erase_orphan ON users FOR DELETE
    USING (
        app_current_tenant() IS NULL
        AND NOT EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = users.id)
    );
-- +goose StatementEnd

-- +goose Down
DROP POLICY users_erase_orphan ON users;
DROP POLICY users_orphan_visible ON users;
DROP POLICY memberships_visible_unbound ON memberships;
