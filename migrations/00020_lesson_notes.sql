-- +goose Up

-- A learner's private note on a lesson.
--
-- One per learner per lesson: a note is a running margin, not a thread, so the
-- learner edits the same one rather than stacking new ones. It is nobody's but
-- theirs — not the author's, not another learner's — which the service enforces
-- by always scoping to the caller; RLS below is the tenant net under that.
--
-- No title, no formatting: it is the digital equivalent of writing in the margin,
-- and a margin has neither.
CREATE TABLE lesson_notes (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    lesson_id uuid NOT NULL REFERENCES lessons (id) ON DELETE CASCADE,

    body text NOT NULL,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- The margin a learner returns to: one note per lesson, looked up by exactly this
-- key on every read and write.
CREATE UNIQUE INDEX lesson_notes_one_per_learner_key
    ON lesson_notes (tenant_id, user_id, lesson_id);

-- Row-level security. ENABLE alone exempts the table owner, which is the role the
-- application connects as; FORCE subjects the owner too.
--
-- Tenant isolation only. That a note belongs to one learner is not a tenancy
-- question — a workspace holds many learners' notes — and is enforced in the
-- service, which never reads or writes a note without the caller's own user id.
ALTER TABLE lesson_notes ENABLE ROW LEVEL SECURITY;
ALTER TABLE lesson_notes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON lesson_notes
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE lesson_notes;
