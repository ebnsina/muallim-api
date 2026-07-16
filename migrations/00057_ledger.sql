-- +goose Up

-- Accounting ledger: a school's own books. A category is a named income or expense
-- head (tuition income, salaries, utilities); an entry is one dated amount posted
-- against it. Money is bigint minor units + currency, defaulting to BDT poisha,
-- never a float — the same rule fees and commerce follow.
--
-- This is the institution's own income and expense record. It is NOT `fees`, which
-- bills students, and NOT `commerce`, which is a learner buying a course. Nothing
-- here moves money; it records money that moved.

CREATE TABLE ledger_categories (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    name       text NOT NULL,
    kind       text NOT NULL CHECK (kind IN ('income', 'expense')),

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- The category list, by kind then name.
CREATE INDEX ledger_categories_tenant_idx ON ledger_categories (tenant_id, kind, name);

CREATE TABLE ledger_entries (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    category_id uuid NOT NULL REFERENCES ledger_categories (id) ON DELETE CASCADE,

    amount      bigint NOT NULL CHECK (amount >= 0),
    currency    char(3) NOT NULL DEFAULT 'BDT',
    occurred_on date NOT NULL,
    description text,

    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- The entry list, newest first — the keyset shape. Covers filter (tenant) and sort
-- (occurred_on, id), so the page is an index scan with no Sort node.
CREATE INDEX ledger_entries_tenant_idx ON ledger_entries (tenant_id, occurred_on DESC, id DESC);
-- One category's entries, same sort — for the category filter and the FK.
CREATE INDEX ledger_entries_category_idx ON ledger_entries (tenant_id, category_id, occurred_on DESC, id DESC);

ALTER TABLE ledger_categories ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_categories FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ledger_categories
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE ledger_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_entries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ledger_entries
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS ledger_entries;
DROP TABLE IF EXISTS ledger_categories;
