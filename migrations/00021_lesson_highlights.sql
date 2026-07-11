-- +goose Up

-- A passage a learner marked in a lesson, with an optional note against it.
--
-- Unlike the whole-lesson note, there are many of these per lesson: a learner
-- highlights this sentence and that paragraph, each with its own remark. It is
-- theirs alone, like the note — the service always scopes to the caller, and RLS
-- below is the tenant net under that.
--
-- The passage is pinned two ways, on purpose. `start_offset`/`end_offset` are
-- character positions into the lesson's text as it stood when the mark was made,
-- and `quote` is the text that was under them. Offsets place the mark exactly
-- while the lesson is unchanged; the quote is what lets a client recognise the
-- passage — or notice it has moved — after an author edits the lesson.
CREATE TABLE lesson_highlights (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    lesson_id uuid NOT NULL REFERENCES lessons (id) ON DELETE CASCADE,

    quote text NOT NULL,
    note  text NOT NULL DEFAULT '',

    start_offset integer NOT NULL,
    end_offset   integer NOT NULL,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- A mark covers at least one character, and starts at or after the beginning.
    -- The database refuses a range that could never have come from a selection.
    CONSTRAINT lesson_highlights_range CHECK (start_offset >= 0 AND end_offset > start_offset)
);

-- A learner's marks on a lesson, read in the order they sit in the text.
CREATE INDEX lesson_highlights_learner_lesson_idx
    ON lesson_highlights (tenant_id, user_id, lesson_id, start_offset, id);

-- Row-level security. ENABLE alone exempts the table owner, which is the role the
-- application connects as; FORCE subjects the owner too. Tenant isolation only;
-- that a mark belongs to one learner is the service's job.
ALTER TABLE lesson_highlights ENABLE ROW LEVEL SECURITY;
ALTER TABLE lesson_highlights FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON lesson_highlights
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE lesson_highlights;
