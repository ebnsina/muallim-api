-- +goose Up

-- Automations: a workspace's own rule for "when this happens, email the learner
-- that". Until now every message this system sent was one of four the code
-- decided on — a verification, a reset, an invitation, a digest — so a school
-- that wanted to welcome its learners had to ask a person to remember to.
--
-- A rule is a template, not a message: the event names when it fires, and the
-- subject and body carry {{placeholders}} the sender fills from what actually
-- happened. Which placeholders exist is the domain's business, enumerated in Go
-- and never free text — a template naming something nobody can resolve is a
-- promise the email breaks.
--
-- The event set is closed by a CHECK, because a rule for an event nothing fires
-- is a rule that silently never runs, and an admin has no way of finding out.

CREATE TABLE automation_rules (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    event      text NOT NULL
               CHECK (event IN ('learner.enrolled', 'course.completed')),
    subject    text NOT NULL,
    body       text NOT NULL,

    -- Off is a first-class state: an admin drafting a welcome email should not
    -- have it sent to the next person who enrols mid-sentence.
    enabled    boolean NOT NULL DEFAULT false,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- The firing path: every enrolment and every completion asks this question, so it
-- is the one query that must never scan. Partial on `enabled`, because a disabled
-- rule is never fired and there is no reason to walk past it.
CREATE INDEX automation_rules_firing_idx
    ON automation_rules (tenant_id, event) WHERE enabled;

-- The admin's list, newest first. Keyset-covered like every other listing.
CREATE INDEX automation_rules_list_idx
    ON automation_rules (tenant_id, created_at DESC, id DESC);

ALTER TABLE automation_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE automation_rules FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON automation_rules
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS automation_rules;
