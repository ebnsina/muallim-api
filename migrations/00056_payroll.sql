-- +goose Up

-- Payroll. A salary structure is what a school pays one staff member — a basic
-- amount plus allowances, less recurring deductions; a payslip is one month's pay
-- computed from it. Money is bigint minor units + currency, defaulting to BDT
-- poisha, never a float — the same rule fees and the gateway ledger follow.
--
-- This is the school paying its own staff. It is unrelated to `commerce` (a learner
-- buying a course) and to `fees` (the school billing its students).

CREATE TABLE payroll_salary_structures (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    staff_id           uuid NOT NULL REFERENCES staff (id) ON DELETE CASCADE,

    basic_amount       bigint NOT NULL CHECK (basic_amount >= 0),
    allowances_amount  bigint NOT NULL DEFAULT 0 CHECK (allowances_amount >= 0),
    deductions_amount  bigint NOT NULL DEFAULT 0 CHECK (deductions_amount >= 0),
    currency           char(3) NOT NULL DEFAULT 'BDT',
    effective_from     date,

    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);

-- One current salary per staff member: setting it again updates in place.
CREATE UNIQUE INDEX payroll_salary_staff_key ON payroll_salary_structures (tenant_id, staff_id);

CREATE TABLE payroll_payslips (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    staff_id           uuid NOT NULL REFERENCES staff (id) ON DELETE CASCADE,

    -- The pay period this slip covers ('2026-01'), so generating the same month is
    -- idempotent, not a second slip.
    period             text NOT NULL,
    gross_amount       bigint NOT NULL CHECK (gross_amount >= 0),
    deductions_amount  bigint NOT NULL DEFAULT 0 CHECK (deductions_amount >= 0),
    net_amount         bigint NOT NULL CHECK (net_amount >= 0),
    currency           char(3) NOT NULL DEFAULT 'BDT',

    status             text NOT NULL DEFAULT 'draft'
                       CHECK (status IN ('draft', 'paid')),
    generated_at       timestamptz NOT NULL DEFAULT now(),
    paid_at            timestamptz,
    method             text NOT NULL DEFAULT '',

    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);

-- One payslip per staff member per period: batch generation conflicts on this so
-- re-running a month's payroll pays nobody twice.
CREATE UNIQUE INDEX payroll_payslips_period_key ON payroll_payslips (tenant_id, staff_id, period);
-- The workspace-wide payslip board, newest first — the keyset list index.
CREATE INDEX payroll_payslips_tenant_idx ON payroll_payslips (tenant_id, created_at DESC, id DESC);
-- One staff member's payslips, newest first.
CREATE INDEX payroll_payslips_staff_idx ON payroll_payslips (tenant_id, staff_id, created_at DESC, id DESC);
-- The month's payroll run by status.
CREATE INDEX payroll_payslips_period_status_idx ON payroll_payslips (tenant_id, period, status);

ALTER TABLE payroll_salary_structures ENABLE ROW LEVEL SECURITY;
ALTER TABLE payroll_salary_structures FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON payroll_salary_structures
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE payroll_payslips ENABLE ROW LEVEL SECURITY;
ALTER TABLE payroll_payslips FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON payroll_payslips
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS payroll_payslips;
DROP TABLE IF EXISTS payroll_salary_structures;
