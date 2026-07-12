-- +goose Up

-- What a course landing page needs and a summary cannot carry: the long pitch,
-- what the learner walks away able to do, and what they need before they start.
ALTER TABLE courses
    ADD COLUMN description  text   NOT NULL DEFAULT '',
    ADD COLUMN objectives   text[] NOT NULL DEFAULT '{}',
    ADD COLUMN requirements text[] NOT NULL DEFAULT '{}',
    ADD COLUMN language     text   NOT NULL DEFAULT 'en',

    -- The author, at last. Until now the creator lived only in the audit log,
    -- which is the wrong place to read from on a page. ON DELETE SET NULL: erasing
    -- a user must not take their courses with them.
    ADD COLUMN created_by   uuid REFERENCES users (id) ON DELETE SET NULL;

-- The landing page joins users through this. Every other read of a course goes by
-- slug and never touches it.
CREATE INDEX courses_created_by_idx ON courses (tenant_id, created_by)
    WHERE created_by IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS courses_created_by_idx;

ALTER TABLE courses
    DROP COLUMN description,
    DROP COLUMN objectives,
    DROP COLUMN requirements,
    DROP COLUMN language,
    DROP COLUMN created_by;
