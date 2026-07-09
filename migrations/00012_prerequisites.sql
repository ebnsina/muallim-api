-- +goose Up

-- A course may require other courses to be finished before somebody enrols on
-- it.
--
-- The edge is stored once per (course, requirement). Both ends are courses in
-- the same workspace: the foreign keys and the tenant policy see to that, and a
-- prerequisite in another workspace would be a course the learner cannot even
-- see.
CREATE TABLE course_prerequisites (
    tenant_id          uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    course_id          uuid NOT NULL REFERENCES courses (id) ON DELETE CASCADE,
    requires_course_id uuid NOT NULL REFERENCES courses (id) ON DELETE CASCADE,
    created_at         timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, course_id, requires_course_id),

    -- A course cannot require itself. The longer cycles — A needs B needs A —
    -- cannot be expressed as a row constraint and are refused by a reachability
    -- check when the edge is added.
    CONSTRAINT course_prerequisites_not_self CHECK (course_id <> requires_course_id)
);

-- The primary key already serves "what does this course require". This index
-- serves the other direction — "what depends on this course" — which is what a
-- cycle check walks and what an author needs before deleting a course.
CREATE INDEX course_prerequisites_requires_idx
    ON course_prerequisites (tenant_id, requires_course_id);

ALTER TABLE course_prerequisites ENABLE ROW LEVEL SECURITY;
ALTER TABLE course_prerequisites FORCE ROW LEVEL SECURITY;

-- One policy covers SELECT, INSERT, and DELETE. A FORCE RLS table denies every
-- command it has no policy for, silently, by matching zero rows.
CREATE POLICY tenant_isolation ON course_prerequisites
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE course_prerequisites;
