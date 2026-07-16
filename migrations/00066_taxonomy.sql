-- +goose Up

-- Course taxonomy: how the catalogue is browsed and filtered. A course sits in at
-- most one category (a section of the catalogue) and carries any number of tags
-- (cross-cutting labels). This is a self-contained domain: it references
-- courses(id) by FK from its own link tables and never touches the courses table
-- itself, so the catalog domain owns the course and this domain owns the labels.

CREATE TABLE course_categories (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    name       text NOT NULL,
    slug       text NOT NULL,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- One category per slug in a workspace: a clash is the domain's ErrDuplicate.
CREATE UNIQUE INDEX course_categories_slug_key ON course_categories (tenant_id, slug);

CREATE TABLE course_tags (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    name       text NOT NULL,
    slug       text NOT NULL,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX course_tags_slug_key ON course_tags (tenant_id, slug);

-- A course belongs to at most one category. The unique (tenant_id, course_id) is
-- what makes SetCourseCategory a replace: the upsert conflicts on it.
CREATE TABLE course_category_links (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    course_id   uuid NOT NULL REFERENCES courses (id) ON DELETE CASCADE,
    category_id uuid NOT NULL REFERENCES course_categories (id) ON DELETE CASCADE,

    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX course_category_links_course_key
    ON course_category_links (tenant_id, course_id);
-- "Which courses are in category X", covering the filter directly.
CREATE INDEX course_category_links_category_idx
    ON course_category_links (tenant_id, category_id, course_id);

CREATE TABLE course_tag_links (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    course_id  uuid NOT NULL REFERENCES courses (id) ON DELETE CASCADE,
    tag_id     uuid NOT NULL REFERENCES course_tags (id) ON DELETE CASCADE,

    created_at timestamptz NOT NULL DEFAULT now()
);

-- A course carries a tag at most once.
CREATE UNIQUE INDEX course_tag_links_key
    ON course_tag_links (tenant_id, course_id, tag_id);
-- "Which courses carry tag Y", covering the filter directly.
CREATE INDEX course_tag_links_tag_idx
    ON course_tag_links (tenant_id, tag_id, course_id);

ALTER TABLE course_categories ENABLE ROW LEVEL SECURITY;
ALTER TABLE course_categories FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON course_categories
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE course_tags ENABLE ROW LEVEL SECURITY;
ALTER TABLE course_tags FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON course_tags
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE course_category_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE course_category_links FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON course_category_links
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE course_tag_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE course_tag_links FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON course_tag_links
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS course_tag_links;
DROP TABLE IF EXISTS course_category_links;
DROP TABLE IF EXISTS course_tags;
DROP TABLE IF EXISTS course_categories;
