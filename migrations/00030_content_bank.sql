-- +goose Up

-- The question bank: a question authored once and reused across quizzes. It
-- mirrors `questions` minus the quiz it belongs to, plus a category to file it
-- under. Adding a bank question to a quiz copies it, so editing the copy never
-- touches the original.
CREATE TABLE bank_questions (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    type text NOT NULL CHECK (type IN (
        'true_false', 'single_choice', 'multiple_choice', 'fill_blanks',
        'short_answer', 'ordering', 'matching', 'open_ended', 'range')),

    prompt        text    NOT NULL,
    points        integer NOT NULL DEFAULT 1 CHECK (points >= 0),
    explanation   text    NOT NULL DEFAULT '',
    case_sensitive boolean NOT NULL DEFAULT false,
    accepted      jsonb   NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(accepted) = 'array'),

    category text NOT NULL DEFAULT '',

    created_at timestamptz NOT NULL DEFAULT now()
);

-- The bank, newest first, filtered by category. An index scan for the list.
CREATE INDEX bank_questions_idx ON bank_questions (tenant_id, category, created_at DESC, id);

CREATE TABLE bank_question_options (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    bank_question_id uuid NOT NULL REFERENCES bank_questions (id) ON DELETE CASCADE,

    content  text    NOT NULL,
    position integer NOT NULL,

    is_correct    boolean NOT NULL DEFAULT false,
    match_content text    NOT NULL DEFAULT ''
);

CREATE INDEX bank_question_options_idx ON bank_question_options (tenant_id, bank_question_id, position);

ALTER TABLE bank_questions ENABLE ROW LEVEL SECURITY;
ALTER TABLE bank_questions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON bank_questions
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE bank_question_options ENABLE ROW LEVEL SECURITY;
ALTER TABLE bank_question_options FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON bank_question_options
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE bank_question_options;
DROP TABLE bank_questions;
