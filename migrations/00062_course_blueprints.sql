-- +goose Up

-- Course blueprints. A blueprint is a standalone, visual curriculum-design tool:
-- a course-structure sketch the author shapes before (or instead of) committing to
-- real catalogue content. It is deliberately self-contained — it references no
-- course, topic or lesson, and nothing in `catalog` references it. The whole shape
-- lives in one jsonb document so a drag-and-drop editor can save it in one write.
--
-- `structure` is a JSON array of modules, each { id, title, lessons: [...] }, and
-- each lesson { id, title, kind, notes } with kind in
-- (video, text, quiz, assignment, file). The domain validates the shape; the column
-- only guarantees it is an array.

CREATE TABLE course_blueprints (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    name        text NOT NULL,
    description text NOT NULL DEFAULT '',
    structure   jsonb NOT NULL DEFAULT '[]',

    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- The blueprints board, newest first: covers the keyset filter and sort so the
-- listing never leaves a Sort node on the request path.
CREATE INDEX course_blueprints_tenant_idx
    ON course_blueprints (tenant_id, created_at DESC, id DESC);

ALTER TABLE course_blueprints ENABLE ROW LEVEL SECURITY;
ALTER TABLE course_blueprints FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON course_blueprints
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS course_blueprints;
