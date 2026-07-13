-- +goose Up

-- Commerce. The workspace sells; Muallim takes a fee and never holds the money.
--
-- The learner pays the school's own connected gateway account, so the school is
-- the merchant: it owns the tax, the refunds and the disputes, because it owns the
-- relationship. Collecting a learner's money for somebody else's course would make
-- Muallim liable for all three, and in most jurisdictions is money transmission —
-- a licence, not a feature.

-- The school's account with a gateway. One per workspace per gateway.
CREATE TABLE payment_accounts (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    gateway text NOT NULL CHECK (gateway IN ('stripe', 'fake')),

    -- The gateway's own id for the account. Never guessed, never composed.
    external_id text NOT NULL,

    status text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'active', 'restricted')),

    -- What the gateway says it will actually do. An account can exist, be onboarded,
    -- and still be refused charges — so this is asked, not inferred from `status`.
    charges_enabled boolean NOT NULL DEFAULT false,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX payment_accounts_tenant_gateway_key
    ON payment_accounts (tenant_id, gateway);

-- A course's price. Its absence is the free course: every course that exists today
-- keeps working, and "free" is not a zero somebody has to remember to write.
CREATE TABLE course_prices (
    course_id uuid PRIMARY KEY REFERENCES courses (id) ON DELETE CASCADE,
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    -- Minor units and a currency, never a float: 0.1 + 0.2 is not 0.3, and a penny
    -- lost to binary is a penny somebody's accountant will find.
    amount_minor bigint  NOT NULL CHECK (amount_minor > 0),
    currency     char(3) NOT NULL,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX course_prices_tenant_idx ON course_prices (tenant_id, course_id);

-- One order is one learner's attempt to buy one course.
CREATE TABLE orders (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    course_id uuid NOT NULL REFERENCES courses (id) ON DELETE CASCADE,
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- What was charged, as it was at the moment of sale. A price the school later
    -- raises must not rewrite what somebody already paid.
    amount_minor bigint  NOT NULL CHECK (amount_minor > 0),
    currency     char(3) NOT NULL,

    status text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'paid', 'failed', 'refunded')),

    gateway text NOT NULL CHECK (gateway IN ('stripe', 'fake')),

    -- The gateway's id for the checkout. Written when the session is created, and
    -- the key a webhook is matched on.
    external_id text NOT NULL,

    paid_at     timestamptz,
    refunded_at timestamptz,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Webhooks arrive twice and out of order. This index is what makes the second
-- delivery a no-op instead of a second enrolment.
--
-- Partial, and that is not a detail: an order is written *before* the gateway is
-- told anything, so it has no session id yet. Without the `WHERE`, every pending
-- order in the system would collide on the empty string — the second learner to
-- reach a checkout page anywhere would be refused. The tests found this, which is
-- the entire reason they exist.
CREATE UNIQUE INDEX orders_gateway_external_key
    ON orders (gateway, external_id)
    WHERE external_id <> '';

-- A learner's own orders, newest first, and the lookup that answers "have they
-- already paid for this?" without a scan.
CREATE INDEX orders_learner_idx ON orders (tenant_id, user_id, created_at DESC, id DESC);
CREATE INDEX orders_course_idx ON orders (tenant_id, course_id, status);

-- Row-level security. ENABLE alone exempts the table owner, which is the role the
-- application connects as; FORCE subjects the owner too.
ALTER TABLE payment_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_accounts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON payment_accounts
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE course_prices ENABLE ROW LEVEL SECURITY;
ALTER TABLE course_prices FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON course_prices
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE orders FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON orders
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE orders;
DROP TABLE course_prices;
DROP TABLE payment_accounts;
