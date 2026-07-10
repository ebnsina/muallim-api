-- +goose Up

-- What a certificate says.
--
-- Workspace-wide, like a grading scale, and a course may point at one when it
-- wants its own. A workspace with no template at all is ordinary: the domain has
-- a built-in default, and a row exists only when somebody wanted different words.
CREATE TABLE certificate_templates (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    name text NOT NULL,

    -- The heading, and the paragraph under it. `body` carries placeholders:
    -- {{learner}}, {{course}}, {{date}}, {{serial}}. Anything else is left alone,
    -- because a typo should print as a typo rather than as an empty space nobody
    -- notices until it is framed on a wall.
    title text NOT NULL,
    body  text NOT NULL,

    -- Who signs it. A certificate signed by nobody is a receipt.
    signatory text NOT NULL DEFAULT '',

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX certificate_templates_tenant_name_key
    ON certificate_templates (tenant_id, lower(name));

-- Which template a course prints. NULL is the built-in default.
-- `ON DELETE SET NULL`: deleting a template must not delete the courses using it.
ALTER TABLE courses
    ADD COLUMN certificate_template_id uuid
        REFERENCES certificate_templates (id) ON DELETE SET NULL;

/*
A certificate that was issued.

The learner's name and the course's title are copied in, not joined to. A person
who changes their name has not invalidated the certificate they earned under the
old one, and a course renamed in 2029 did not retroactively rename what somebody
completed in 2026. The row is a record of an event, and an event does not change.

The template is remembered by its id and by its words, for the same reason: the
words on the certificate are the words that were on it.
*/
CREATE TABLE certificates (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    course_id uuid NOT NULL REFERENCES courses (id) ON DELETE CASCADE,
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    /*
        The public name of this certificate.

        Unique across the installation, not the workspace: it is looked up without a
        session, on a page anybody may open, and a code that meant one thing here and
        another there would verify the wrong certificate for whoever guessed first.

        Unguessable, not sequential. A serial anybody can increment is a directory of
        everybody who has ever finished a course, and their names.
    */
    serial text NOT NULL,

    -- The event, as it was. Copied, never joined.
    learner_name text        NOT NULL,
    course_title text        NOT NULL,
    issued_at    timestamptz NOT NULL DEFAULT now(),

    -- The words it was printed with, and which template they came from. The id is
    -- kept for reporting and may go; the words are what the certificate says.
    template_id   uuid REFERENCES certificate_templates (id) ON DELETE SET NULL,
    title         text NOT NULL,
    body          text NOT NULL,
    signatory     text NOT NULL DEFAULT '',

    /*
        A certificate is revoked, never deleted.

        Somebody has the code, and a code that stops resolving is indistinguishable
        from a code that was never real. Revoked, the page says so, which is the
        answer whoever is checking actually wants.
    */
    revoked_at timestamptz,
    revoked_reason text NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX certificates_serial_key ON certificates (serial);

-- One certificate per learner per course. Finishing a course twice is finishing
-- it once.
CREATE UNIQUE INDEX certificates_one_per_learner_key ON certificates (tenant_id, course_id, user_id);

-- "My certificates", newest first.
CREATE INDEX certificates_learner_idx ON certificates (tenant_id, user_id, issued_at DESC, id);

-- "Everybody who has finished this course."
CREATE INDEX certificates_course_idx ON certificates (tenant_id, course_id, issued_at DESC, id);

-- Row-level security. ENABLE alone exempts the table owner, which is the role the
-- application connects as; FORCE subjects the owner too.
--
-- These policies isolate tenants and nothing else. A certificate is verified
-- without a session, and the request that verifies it has resolved a workspace
-- from its host — so the lookup runs inside a tenant like every other read. That a
-- learner may not read another learner's certificate is not a tenancy question,
-- and is enforced in the service.
-- +goose StatementBegin
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['certificate_templates', 'certificates'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format(
            'CREATE POLICY tenant_isolation ON %I
                 USING (tenant_id = app_current_tenant())
                 WITH CHECK (tenant_id = app_current_tenant())', t);
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE certificates;
ALTER TABLE courses DROP COLUMN certificate_template_id;
DROP TABLE certificate_templates;
