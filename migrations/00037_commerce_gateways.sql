-- +goose Up

-- A refund is issued against the gateway's *payment*, not against the checkout
-- session that created it: the session is how the learner got to the payment, and
-- several gateways forget it the moment the money moves. So the payment's own id is
-- kept when the gateway tells us about it, and the refund's id when one is made.
ALTER TABLE orders
    ADD COLUMN payment_external_id text NOT NULL DEFAULT '',
    ADD COLUMN refund_external_id  text NOT NULL DEFAULT '';

/*
    A workspace's credentials with a gateway.

    Stripe Connect does not need this: the platform holds one key and acts *on behalf
    of* the connected account. SSLCommerz and bKash have no such notion — each school
    is its own merchant with its own store id and secret — so those secrets have to
    live here, and they are the one thing in this database that must never be readable
    from a dump.

    Hence `secret_cipher`: AES-256-GCM under a key that is not in the database. The
    nonce is stored with it, prefixed, because a nonce is not a secret; the key is,
    and it comes from the environment.
*/
CREATE TABLE payment_credentials (
    account_id uuid PRIMARY KEY REFERENCES payment_accounts (id) ON DELETE CASCADE,
    tenant_id  uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    -- The public half: a store id, an app key. Not secret, and needed to build a request.
    public_id text NOT NULL,

    -- The secret half, encrypted. Never selected into a log, never returned by the API.
    secret_cipher bytea NOT NULL,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE payment_credentials ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_credentials FORCE ROW LEVEL SECURITY;

CREATE POLICY payment_credentials_tenant_isolation ON payment_credentials
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE payment_credentials;

ALTER TABLE orders
    DROP COLUMN payment_external_id,
    DROP COLUMN refund_external_id;
