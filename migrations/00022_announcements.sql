-- +goose Up

-- An announcement an instructor posts to a course.
--
-- Course-scoped and authored with course:write, like the curriculum — so it lives
-- with the course and is gone when the course is. Read by whoever may see the
-- course: a learner on a published one, the author on a draft.
--
-- The author is remembered but may go: `ON DELETE SET NULL` keeps the notice on
-- the board when the person who pinned it leaves, because the announcement was
-- made to the course, not by a row that has to survive.
CREATE TABLE announcements (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    course_id uuid NOT NULL REFERENCES courses (id) ON DELETE CASCADE,
    author_id uuid REFERENCES users (id) ON DELETE SET NULL,

    title text NOT NULL,
    body  text NOT NULL,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- A course's announcements, newest first — the order a board is read.
CREATE INDEX announcements_course_idx
    ON announcements (tenant_id, course_id, created_at DESC, id);

-- Row-level security. ENABLE alone exempts the table owner, which is the role the
-- application connects as; FORCE subjects the owner too. Tenant isolation only;
-- who may read a course's announcements is the service's decision, from the
-- course's own visibility.
ALTER TABLE announcements ENABLE ROW LEVEL SECURITY;
ALTER TABLE announcements FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON announcements
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE announcements;
