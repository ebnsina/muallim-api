-- +goose Up

-- A learner's public question on a lesson, and the answers it draws.
--
-- Unlike a note or a highlight, a question is seen by everyone studying the
-- course: it is the one thing the learn domain owns that is shared rather than
-- private. Who may see it follows who may read the lesson — enrolled, previewing,
-- or authoring — enforced in the query by a join to enrolments, the same
-- shared-schema read the course-wide note and highlight lists already use.
--
-- The author is remembered but may go: `ON DELETE SET NULL` keeps the thread
-- intact when an account is erased, and the row is shown without a name.
CREATE TABLE lesson_questions (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    lesson_id uuid NOT NULL REFERENCES lessons (id) ON DELETE CASCADE,
    author_id uuid REFERENCES users (id) ON DELETE SET NULL,

    body text NOT NULL,

    created_at timestamptz NOT NULL DEFAULT now()
);

-- A lesson's questions, newest first — the order a board is read.
CREATE INDEX lesson_questions_lesson_idx
    ON lesson_questions (tenant_id, lesson_id, created_at DESC, id);

CREATE TABLE lesson_answers (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    question_id uuid NOT NULL REFERENCES lesson_questions (id) ON DELETE CASCADE,
    author_id   uuid REFERENCES users (id) ON DELETE SET NULL,

    body text NOT NULL,

    -- Whether the answerer could author the course when they wrote it. Recorded at
    -- write time, not inferred on read: a teaching assistant who later loses the
    -- role still gave an instructor's answer, and the badge should not move.
    by_instructor boolean NOT NULL DEFAULT false,

    created_at timestamptz NOT NULL DEFAULT now()
);

-- A question's answers, oldest first — the order a conversation is read.
CREATE INDEX lesson_answers_question_idx
    ON lesson_answers (tenant_id, question_id, created_at, id);

-- Row-level security. ENABLE alone exempts the table owner, the role the
-- application connects as; FORCE subjects the owner too. Tenant isolation is the
-- net; who within a tenant may read a thread is the service's join to enrolments.
ALTER TABLE lesson_questions ENABLE ROW LEVEL SECURITY;
ALTER TABLE lesson_questions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON lesson_questions
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE lesson_answers ENABLE ROW LEVEL SECURITY;
ALTER TABLE lesson_answers FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON lesson_answers
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE lesson_answers;
DROP TABLE lesson_questions;
