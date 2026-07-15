-- +goose Up

-- Institutional fees. A fee structure is a recurring charge a school defines
-- (monthly tuition for a class, an annual admission fee); an invoice is one charge
-- raised against one student. Money is bigint minor units + currency, defaulting to
-- BDT poisha, never a float — the same rule the gateway ledger follows.
--
-- This is billing the school raises against its own students; it is NOT the course
-- checkout in `commerce`, which is a learner buying a course. The school is the
-- merchant of record there and here alike: Muallim never holds the money.

CREATE TABLE fee_structures (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    name           text NOT NULL,
    amount         bigint NOT NULL CHECK (amount >= 0),
    currency       char(3) NOT NULL DEFAULT 'BDT',
    -- Which class this fee applies to, if it is class-wide. Null is a fee raised
    -- ad hoc against individual students.
    grade_level_id uuid REFERENCES grade_levels (id) ON DELETE SET NULL,
    recurrence     text NOT NULL DEFAULT 'one_time'
                   CHECK (recurrence IN ('one_time', 'monthly', 'termly', 'annual')),

    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX fee_structures_tenant_idx ON fee_structures (tenant_id, name);

CREATE TABLE fee_invoices (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    student_id        uuid NOT NULL REFERENCES students (id) ON DELETE CASCADE,
    fee_structure_id  uuid REFERENCES fee_structures (id) ON DELETE SET NULL,

    title             text NOT NULL,
    amount            bigint NOT NULL CHECK (amount >= 0),
    currency          char(3) NOT NULL DEFAULT 'BDT',
    -- The billing period a recurring fee covers ('2026-01', '2026-T1', '2026'),
    -- so re-issuing the same month is idempotent, not a second charge.
    period            text NOT NULL DEFAULT '',
    due_date          date,

    status            text NOT NULL DEFAULT 'unpaid'
                      CHECK (status IN ('unpaid', 'paid', 'waived', 'cancelled')),
    paid_amount       bigint NOT NULL DEFAULT 0 CHECK (paid_amount >= 0),
    paid_at           timestamptz,
    method            text NOT NULL DEFAULT '',
    note              text NOT NULL DEFAULT '',

    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- One invoice per student per structure per period: the batch issue conflicts on
-- this so re-running a month's billing enrols nobody twice. Only for structure-backed
-- invoices; ad-hoc ones (null structure) are never deduplicated.
CREATE UNIQUE INDEX fee_invoices_period_key
    ON fee_invoices (tenant_id, student_id, fee_structure_id, period)
    WHERE fee_structure_id IS NOT NULL;
-- A student's ledger, and the school's outstanding-dues report by due date.
CREATE INDEX fee_invoices_student_idx ON fee_invoices (tenant_id, student_id, created_at DESC, id DESC);
CREATE INDEX fee_invoices_due_idx ON fee_invoices (tenant_id, status, due_date);

ALTER TABLE fee_structures ENABLE ROW LEVEL SECURITY;
ALTER TABLE fee_structures FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON fee_structures
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE fee_invoices ENABLE ROW LEVEL SECURITY;
ALTER TABLE fee_invoices FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON fee_invoices
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS fee_invoices;
DROP TABLE IF EXISTS fee_structures;
