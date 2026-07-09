-- +goose Up

-- app_current_tenant reads the tenant bound to the current transaction by
-- database.WithTenant. It is the sole input to every row-level security policy.
--
-- The `true` argument makes a missing setting return NULL rather than raise, and
-- NULL never equals a tenant_id. Forgetting to bind a tenant therefore yields
-- zero rows rather than every row: the failure mode is an empty page, not a data
-- leak.
-- +goose StatementBegin
CREATE FUNCTION app_current_tenant() RETURNS uuid
    LANGUAGE sql
    STABLE
    PARALLEL SAFE
AS $$
    SELECT nullif(current_setting('app.tenant_id', true), '')::uuid
$$;
-- +goose StatementEnd

-- Tenants are the isolation boundary itself, so this table is not tenant-scoped
-- and carries no RLS policy.
CREATE TABLE tenants (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    subdomain   text        NOT NULL,
    custom_domain text,
    name        text        NOT NULL,
    status      text        NOT NULL DEFAULT 'active'
                CHECK (status IN ('active', 'suspended', 'cancelled')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Host resolution happens on every single request, so both lookup paths are
-- unique indexes rather than plain ones.
CREATE UNIQUE INDEX tenants_subdomain_key ON tenants (lower(subdomain));
CREATE UNIQUE INDEX tenants_custom_domain_key ON tenants (lower(custom_domain))
    WHERE custom_domain IS NOT NULL;

-- +goose Down
DROP TABLE tenants;
DROP FUNCTION app_current_tenant();
