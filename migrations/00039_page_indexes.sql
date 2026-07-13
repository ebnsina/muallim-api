-- +goose Up

/*
    The indexes the new pages are read on.

    A keyset without an index covering both the filter and the sort is an OFFSET
    that lies about itself: Postgres reads every membership in the workspace,
    sorts them, and throws away all but fifty. The plan says so — a Sort node on a
    request path — and this is what removes it.

    Invitations already had theirs. Certificates had one per course and one per
    learner, and none for "everything this workspace has issued", which is exactly
    the question a registrar asks.
*/
CREATE INDEX memberships_tenant_joined_idx
    ON memberships (tenant_id, created_at, id);

CREATE INDEX certificates_tenant_issued_idx
    ON certificates (tenant_id, issued_at DESC, id DESC);

-- +goose Down
DROP INDEX memberships_tenant_joined_idx;
DROP INDEX certificates_tenant_issued_idx;
