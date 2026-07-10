-- +goose Up

-- A quiz belongs to a lesson, and a lesson has at most one.
--
-- Attaching it to the lesson rather than to the course means every rule the
-- curriculum already enforces — drafts are invisible, drip holds a lesson back,
-- a preview is readable without enrolling — governs the quiz for free, and none
-- of it has to be restated here.
CREATE TABLE quizzes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    lesson_id   uuid        NOT NULL REFERENCES lessons (id) ON DELETE CASCADE,
    title       text        NOT NULL,
    description text        NOT NULL DEFAULT '',

    -- Zero means no limit, in both cases. NULL would say the same thing less
    -- clearly and would have to be coalesced at every read.
    time_limit_seconds integer NOT NULL DEFAULT 0 CHECK (time_limit_seconds >= 0),
    max_attempts       integer NOT NULL DEFAULT 0 CHECK (max_attempts >= 0),

    passing_percent integer NOT NULL DEFAULT 0
        CHECK (passing_percent BETWEEN 0 AND 100),

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX quizzes_tenant_lesson_key ON quizzes (tenant_id, lesson_id);

CREATE TABLE questions (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    quiz_id   uuid NOT NULL REFERENCES quizzes (id) ON DELETE CASCADE,

    type text NOT NULL CHECK (type IN (
        'true_false', 'single_choice', 'multiple_choice', 'fill_blanks',
        'short_answer', 'ordering', 'matching', 'open_ended')),

    prompt   text    NOT NULL,
    points   integer NOT NULL DEFAULT 1 CHECK (points >= 0),
    position integer NOT NULL,

    -- Shown once the attempt is graded, never before. Tutor LMS has never shipped
    -- this; it is the thing its users ask for most.
    explanation text NOT NULL DEFAULT '',

    case_sensitive boolean NOT NULL DEFAULT false,

    -- One array of accepted spellings per blank, in order:
    -- [["4", "four"], ["Paris"]]. Empty for every type but fill_blanks and
    -- short_answer. A jsonb here rather than a table because nothing ever queries
    -- an individual spelling — the whole array is read at grading time and never
    -- filtered, joined, or counted.
    accepted jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(accepted) = 'array'),

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE question_options (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    question_id uuid NOT NULL REFERENCES questions (id) ON DELETE CASCADE,

    content  text    NOT NULL,
    position integer NOT NULL,

    -- The answer, for the choice types. Never selected into a learner's view.
    is_correct boolean NOT NULL DEFAULT false,

    -- The right-hand side of a matching pair.
    --
    -- match_id is a separate identifier on purpose. A learner is sent the options
    -- and the matches as two detached lists; if the match's own row id gave it
    -- away, or if match_content arrived inside the option it belongs to, the
    -- question would answer itself.
    match_id      uuid NOT NULL DEFAULT gen_random_uuid(),
    match_content text NOT NULL DEFAULT ''
);

-- Dense, zero-based positions within a parent, as everywhere else. DEFERRABLE
-- because a reorder passes through states where two siblings share a position.
ALTER TABLE questions
    ADD CONSTRAINT questions_quiz_position_key
    UNIQUE (tenant_id, quiz_id, position) DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE question_options
    ADD CONSTRAINT question_options_question_position_key
    UNIQUE (tenant_id, question_id, position) DEFERRABLE INITIALLY DEFERRED;

CREATE INDEX questions_tenant_quiz_position_idx
    ON questions (tenant_id, quiz_id, position, id);
CREATE INDEX question_options_tenant_question_position_idx
    ON question_options (tenant_id, question_id, position, id);

-- An attempt is one learner's run at a quiz.
CREATE TABLE quiz_attempts (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    quiz_id   uuid NOT NULL REFERENCES quizzes (id) ON DELETE CASCADE,
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- Counts from one, per learner per quiz. Shown to them, and to an instructor
    -- deciding whether to grant another go.
    number integer NOT NULL CHECK (number >= 1),

    status text NOT NULL DEFAULT 'in_progress'
        CHECK (status IN ('in_progress', 'grading', 'awaiting_review', 'graded')),

    started_at   timestamptz NOT NULL DEFAULT now(),
    submitted_at timestamptz,
    graded_at    timestamptz,

    -- started_at plus the quiz's time limit, frozen at the moment the attempt
    -- began. Recomputing it from the quiz at read time would let an author
    -- shorten an attempt already under way.
    expires_at timestamptz,

    -- max_points is the sum of the quiz's question points as they stood when the
    -- attempt was graded, recorded here so that editing the quiz afterwards
    -- cannot silently restate an old result.
    points     integer NOT NULL DEFAULT 0 CHECK (points >= 0),
    max_points integer NOT NULL DEFAULT 0 CHECK (max_points >= 0),

    -- NULL until every question has a grade. An essay awaiting an instructor
    -- leaves it NULL, because a pass is not a thing to guess at.
    passed boolean
);

CREATE UNIQUE INDEX quiz_attempts_tenant_quiz_user_number_key
    ON quiz_attempts (tenant_id, quiz_id, user_id, number);

-- One live attempt per learner per quiz. Two browser tabs pressing Start at once
-- is a race, and this is where it is decided — not in a read-then-write that both
-- tabs win.
CREATE UNIQUE INDEX quiz_attempts_one_in_progress_key
    ON quiz_attempts (tenant_id, quiz_id, user_id)
    WHERE status = 'in_progress';

-- "This learner's attempts at this quiz, newest first" covers the filter and the
-- sort, so listing them needs no sort node.
CREATE INDEX quiz_attempts_tenant_quiz_user_idx
    ON quiz_attempts (tenant_id, quiz_id, user_id, number DESC);

CREATE TABLE attempt_answers (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    attempt_id  uuid NOT NULL REFERENCES quiz_attempts (id) ON DELETE CASCADE,
    question_id uuid NOT NULL REFERENCES questions (id) ON DELETE CASCADE,

    -- The learner's answer, in the one shape that covers every question type.
    -- Opaque to the database: nothing here filters on it.
    response jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(response) = 'object'),

    -- False until something has judged this answer: a job for the objective
    -- types, a person for an essay.
    graded  boolean NOT NULL DEFAULT false,
    correct boolean NOT NULL DEFAULT false,
    points  integer NOT NULL DEFAULT 0 CHECK (points >= 0),

    -- What an instructor wrote when grading by hand.
    feedback  text NOT NULL DEFAULT '',
    graded_at timestamptz,

    updated_at timestamptz NOT NULL DEFAULT now()
);

-- One answer per question per attempt. The learner may change it until they
-- submit, which is an upsert on this key rather than a second row.
CREATE UNIQUE INDEX attempt_answers_tenant_attempt_question_key
    ON attempt_answers (tenant_id, attempt_id, question_id);

-- Row-level security. ENABLE alone exempts the table owner, which is the role the
-- application connects as; FORCE subjects the owner too. Application code always
-- filters by tenant_id explicitly — this is the net for the day someone forgets.
--
-- Note what these policies do *not* say: nothing here stops one learner reading
-- another's attempt. That is not a tenancy question, and an RLS policy that tried
-- to answer it would need the current user, which the transaction does not carry.
-- Ownership is enforced in the service, where the actor is known.
-- +goose StatementBegin
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['quizzes', 'questions', 'question_options',
                             'quiz_attempts', 'attempt_answers'] LOOP
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
DROP TABLE attempt_answers;
DROP TABLE quiz_attempts;
DROP TABLE question_options;
DROP TABLE questions;
DROP TABLE quizzes;
