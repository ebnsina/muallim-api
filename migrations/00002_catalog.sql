-- +goose Up

CREATE TABLE courses (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    slug         text        NOT NULL,
    title        text        NOT NULL,
    summary      text        NOT NULL DEFAULT '',
    difficulty   text        NOT NULL DEFAULT 'beginner'
                 CHECK (difficulty IN ('beginner', 'intermediate', 'advanced', 'expert')),
    status       text        NOT NULL DEFAULT 'draft'
                 CHECK (status IN ('draft', 'published', 'archived')),
    published_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE topics (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    course_id  uuid        NOT NULL REFERENCES courses (id) ON DELETE CASCADE,
    title      text        NOT NULL,
    position   integer     NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE lessons (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    topic_id     uuid        NOT NULL REFERENCES topics (id) ON DELETE CASCADE,
    title        text        NOT NULL,
    content_type text        NOT NULL DEFAULT 'text'
                 CHECK (content_type IN ('text', 'video', 'quiz', 'assignment', 'live', 'scorm', 'h5p')),
    duration_seconds integer NOT NULL DEFAULT 0 CHECK (duration_seconds >= 0),
    is_preview   boolean     NOT NULL DEFAULT false,
    position     integer     NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- Slugs are unique per tenant, not globally: two schools may both teach "algebra-1".
CREATE UNIQUE INDEX courses_tenant_slug_key ON courses (tenant_id, lower(slug));

-- The catalog list endpoint keyset-paginates published courses by
-- (created_at, id) descending. This index covers the filter and the sort, so the
-- plan is an index scan with no sort node, and page 500 costs what page 1 costs.
CREATE INDEX courses_tenant_status_created_idx
    ON courses (tenant_id, status, created_at DESC, id DESC);

-- Curriculum loads fetch every topic of one course, and every lesson of those
-- topics, in exactly two batched queries. These indexes make both an index scan
-- and return rows already ordered, so the service never sorts in Go.
CREATE INDEX topics_tenant_course_position_idx
    ON topics (tenant_id, course_id, position, id);
CREATE INDEX lessons_tenant_topic_position_idx
    ON lessons (tenant_id, topic_id, position, id);

-- Row-level security. ENABLE alone exempts the table owner, which is the role
-- the application connects as — so it would protect nothing here. FORCE subjects
-- the owner to the policy too.
--
-- Application code always filters by tenant_id explicitly. This is the net for
-- the day someone forgets, not the primary control.
-- +goose StatementBegin
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['courses', 'topics', 'lessons'] LOOP
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
DROP TABLE lessons;
DROP TABLE topics;
DROP TABLE courses;
