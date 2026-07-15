-- +goose Up

-- Staff: the people who run the institution — teachers, and the office around them.
-- A staff record may link to a user account (a teacher who logs in) or stand alone
-- (a record the office keeps for someone with no login yet). The role is what they
-- do here; it is not a login permission, which lives on the membership.

CREATE TABLE staff (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    staff_no   text NOT NULL DEFAULT '',
    full_name  text NOT NULL,
    role       text NOT NULL DEFAULT 'teacher'
               CHECK (role IN ('teacher', 'principal', 'admin', 'accountant', 'librarian', 'support')),

    email      text NOT NULL DEFAULT '',
    phone      text NOT NULL DEFAULT '',

    -- The user account this record belongs to, if the person logs in. Null is a
    -- record with no login.
    user_id    uuid REFERENCES users (id) ON DELETE SET NULL,

    status     text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive')),
    joined_on  date,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- A staff number is unique when given; blank is allowed and not deduplicated.
CREATE UNIQUE INDEX staff_no_key ON staff (tenant_id, staff_no) WHERE staff_no <> '';
-- The roster, by name, and narrowed to a role — each keyset shape gets its index.
CREATE INDEX staff_tenant_name_idx ON staff (tenant_id, full_name, id);
CREATE INDEX staff_role_name_idx ON staff (tenant_id, role, full_name, id);

ALTER TABLE staff ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON staff
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS staff;
