-- +goose Up

-- A grading scale turns a percentage into a word.
--
-- Workspace-wide, because a school grades the same way across its catalogue, and
-- a course may point at one when it does not. A workspace with no scale at all is
-- ordinary: the domain has a built-in default, and a scale exists only when
-- somebody wanted a different one.
CREATE TABLE grading_scales (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    name text NOT NULL,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX grading_scales_tenant_name_key ON grading_scales (tenant_id, lower(name));

-- One band of a scale: everything at or above `min_percent`, and below the band
-- above it, is called `label`.
--
-- `min_percent` is an integer. A scale whose boundary is 89.5% is a scale whose
-- author has not decided what happens at 89.5%.
CREATE TABLE grading_bands (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    scale_id  uuid NOT NULL REFERENCES grading_scales (id) ON DELETE CASCADE,

    label       text    NOT NULL,
    min_percent integer NOT NULL CHECK (min_percent BETWEEN 0 AND 100),

    -- True when this band is a pass. Which band that is, is a decision the scale's
    -- author makes; deriving it from a fixed percentage would put the rule in two
    -- places and let them disagree.
    is_pass boolean NOT NULL DEFAULT false
);

-- Two bands cannot start at the same percentage: the letter would depend on which
-- row the planner reached first.
CREATE UNIQUE INDEX grading_bands_scale_floor_key ON grading_bands (tenant_id, scale_id, min_percent);
CREATE INDEX grading_bands_scale_idx ON grading_bands (tenant_id, scale_id, min_percent DESC);

-- Which scale a course grades by. NULL means the workspace's built-in default.
-- `ON DELETE SET NULL`: deleting a scale must not delete the courses that used it.
ALTER TABLE courses
    ADD COLUMN grading_scale_id uuid REFERENCES grading_scales (id) ON DELETE SET NULL;

-- A thing that is worth marks: one quiz, or one assignment.
--
-- Written by the domain that owns the thing, in the transaction that created it.
-- The gradebook does not go looking through `quizzes` and `assignments` for rows
-- to score — it would need to know both schemas, and it would learn about a third
-- kind of assessment by breaking.
CREATE TABLE grade_items (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    course_id uuid NOT NULL REFERENCES courses (id) ON DELETE CASCADE,
    lesson_id uuid NOT NULL REFERENCES lessons (id) ON DELETE CASCADE,

    source text NOT NULL CHECK (source IN ('quiz', 'assignment')),

    -- The quiz or assignment this item scores. No foreign key: it would have to
    -- point at one of two tables. The unique index below is what keeps it honest.
    source_id uuid NOT NULL,

    title      text    NOT NULL,
    max_points integer NOT NULL CHECK (max_points > 0),

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- One item per assessment. A quiz that is edited updates its item; it does not
-- acquire a second one.
CREATE UNIQUE INDEX grade_items_source_key ON grade_items (tenant_id, source, source_id);

-- The gradebook's read path: every item of a course, in lesson order.
CREATE INDEX grade_items_course_idx ON grade_items (tenant_id, course_id, created_at, id);

-- One learner's mark for one item.
--
-- Written in the transaction that graded the thing. A grade recorded afterwards,
-- by a job, is a gradebook that disagrees with the attempt it summarises for as
-- long as the job is queued — and for ever if it fails.
CREATE TABLE grade_entries (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    grade_item_id uuid NOT NULL REFERENCES grade_items (id) ON DELETE CASCADE,
    user_id       uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    points integer NOT NULL CHECK (points >= 0),

    -- Copied from the item as it stood when this was graded. An author who raises
    -- a quiz from 10 points to 20 has not retroactively halved everybody's grade.
    max_points integer NOT NULL CHECK (max_points > 0),

    graded_at timestamptz NOT NULL DEFAULT now(),

    CHECK (points <= max_points)
);

CREATE UNIQUE INDEX grade_entries_one_per_learner_key
    ON grade_entries (tenant_id, grade_item_id, user_id);

-- "Every entry for these items", which is how the gradebook reads a whole course
-- in one query rather than one per learner.
CREATE INDEX grade_entries_item_idx ON grade_entries (tenant_id, grade_item_id, user_id);

-- "This learner's grades in this course", for the page a learner opens.
CREATE INDEX grade_entries_learner_idx ON grade_entries (tenant_id, user_id, grade_item_id);

-- Row-level security. ENABLE alone exempts the table owner, which is the role the
-- application connects as; FORCE subjects the owner too.
--
-- These policies isolate tenants and nothing else. That a learner may not read
-- another learner's grade is not a tenancy question, and a policy that tried to
-- answer it would need the current user, which the transaction does not carry.
-- Ownership is enforced in the service.
-- +goose StatementBegin
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['grading_scales', 'grading_bands', 'grade_items', 'grade_entries'] LOOP
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
DROP TABLE grade_entries;
DROP TABLE grade_items;
ALTER TABLE courses DROP COLUMN grading_scale_id;
DROP TABLE grading_bands;
DROP TABLE grading_scales;
