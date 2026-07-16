-- +goose Up

-- Learning paths: an ordered track of courses a learner works through in
-- sequence. A self-contained domain — it references the catalogue by
-- courses(id) and nothing else, and knows nothing of enrolment or progress.
-- The coordinator wires per-course progress (from enroll) onto the ordered
-- course list this domain owns.
--
-- A path is draft until published, the same two-state visibility a course has.

CREATE TABLE learning_paths (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    slug        text NOT NULL,
    title       text NOT NULL,
    description text NOT NULL DEFAULT '',
    status      text NOT NULL DEFAULT 'draft'
                CHECK (status IN ('draft', 'published')),

    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- A slug is unique within a workspace, and appears in URLs.
CREATE UNIQUE INDEX learning_paths_slug_key ON learning_paths (tenant_id, slug);
-- The list, newest first: the keyset (created_at, id) is covered so no Sort node
-- lands on the request path, unfiltered and filtered-by-status alike.
CREATE INDEX learning_paths_list_idx ON learning_paths (tenant_id, created_at DESC, id DESC);
CREATE INDEX learning_paths_status_idx ON learning_paths (tenant_id, status, created_at DESC, id DESC);

CREATE TABLE learning_path_courses (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    path_id    uuid NOT NULL REFERENCES learning_paths (id) ON DELETE CASCADE,
    course_id  uuid NOT NULL REFERENCES courses (id) ON DELETE CASCADE,
    position   int  NOT NULL,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- A course appears in a path at most once.
ALTER TABLE learning_path_courses
    ADD CONSTRAINT learning_path_courses_course_key
    UNIQUE (tenant_id, path_id, course_id);

-- Dense, zero-based positions within a path. DEFERRABLE because a reorder passes
-- through states where two rows momentarily share a position; the unique index it
-- creates also covers the ordered load (WHERE path_id ORDER BY position).
ALTER TABLE learning_path_courses
    ADD CONSTRAINT learning_path_courses_position_key
    UNIQUE (tenant_id, path_id, position) DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE learning_paths ENABLE ROW LEVEL SECURITY;
ALTER TABLE learning_paths FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON learning_paths
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE learning_path_courses ENABLE ROW LEVEL SECURITY;
ALTER TABLE learning_path_courses FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON learning_path_courses
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS learning_path_courses;
DROP TABLE IF EXISTS learning_paths;
