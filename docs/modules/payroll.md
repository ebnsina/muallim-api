# Payroll

How a school pays its staff: the salary structure it sets per staff member and the
payslips it generates from them. A modular-monolith domain like the rest — it knows
nothing about HTTP, references staff by id, and is tenant-scoped with RLS behind
every table. Money is `bigint` minor units + `currency char(3)`, defaulting to BDT
poisha, never a float.

This is the school paying its own staff. It is unrelated to `commerce` (a learner
buying a course) and to `fees` (the school billing its students).

## Model

- **`payroll_salary_structures`** — what a school pays one staff member. `basic_amount`,
  `allowances_amount` and `deductions_amount` (each `>= 0`), a `currency`, and an
  optional `effective_from`. A unique `(tenant_id, staff_id)` keeps one current
  structure per staff member; setting it again updates that row in place.
- **`payroll_payslips`** — one period's pay for one staff member. A `period`
  (`'2026-01'`), `gross_amount`, `deductions_amount`, `net_amount`, a `currency`, a
  `status` of `draft` or `paid`, `generated_at`, and nullable `paid_at`/`method`.

Generating a period draws one payslip per staff member with a salary structure, in a
single `INSERT ... SELECT` that computes gross (`basic + allowances`), the deduction,
and net (`basic + allowances - deductions`) from the structure. A unique
`(tenant_id, staff_id, period)` with `ON CONFLICT DO NOTHING` makes re-running a
month idempotent — nobody is paid twice. Marking a draft paid is guarded by
`status = 'draft'`, so a repeat is a no-op.

The listing is keyset-paginated, newest first (`created_at DESC, id DESC`); the
by-staff shape has its own covering index, and the period/status filters ride the
same index-ordered scan — no `Sort` node on the request path, no `OFFSET`.

## Endpoints

All under `academics:manage`, admin-only, bearer auth.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v1/payroll/salary/{staff_id}` | A staff member's salary structure. |
| `PUT` | `/v1/payroll/salary/{staff_id}` | Set/replace it. Body: `basic_amount`, `allowances_amount?`, `deductions_amount?`, `currency?`, `effective_from?`. |
| `GET` | `/v1/payroll/payslips` | Payslips, newest first. Filter by `staff_id`, `period` and/or `status`. Keyset `cursor` + `has_more`. |
| `POST` | `/v1/payroll/payslips` | Generate a period. Body: `period`, `staff_id?` (empty runs the whole workspace). Returns `generated`. |
| `POST` | `/v1/payroll/payslips/{id}/pay` | Record that a draft payslip was paid. Body: `method?`. |

## Errors

| Sentinel | Status | When |
| --- | --- | --- |
| `ErrNotFound` | 404 | No such salary structure or payslip in this workspace. |
| `ErrNotDraft` | 409 | The payslip was paid already. |
| `ErrNoSalary` | 422 | Generating for a named staff member who has no salary structure. |
| `ErrInvalidStructure` | 422 | Negative amounts, or deductions exceeding gross pay. |
| `ErrInvalidPayslip` | 422 | A period-less generation request. |
| `ErrInvalidPage` | 422 | The page cursor did not decode. |
