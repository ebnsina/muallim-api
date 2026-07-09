-- +goose Up

-- Lessons need a body. Which columns are meaningful depends on content_type:
-- a text lesson uses `content`, a video lesson uses the video_* columns.
--
-- Not a jsonb blob. A column the database understands can be indexed, checked,
-- and migrated; a blob defers every one of those to code that will forget.
ALTER TABLE lessons
    ADD COLUMN content      text NOT NULL DEFAULT '',
    ADD COLUMN video_source text NOT NULL DEFAULT 'none'
        CHECK (video_source IN ('none', 'youtube', 'vimeo', 'embed', 'hosted')),
    ADD COLUMN video_url    text NOT NULL DEFAULT '';

-- Ordering is dense and zero-based within a parent, maintained by explicit
-- reorder operations rather than by incrementing a counter on insert.
--
-- These indexes already cover (tenant_id, parent, position); the constraint here
-- is the one the reorder statement relies on: a position is unique among its
-- siblings. DEFERRABLE, because a reorder necessarily passes through states where
-- two rows share a position mid-statement.
ALTER TABLE topics
    ADD CONSTRAINT topics_course_position_key
    UNIQUE (tenant_id, course_id, position) DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE lessons
    ADD CONSTRAINT lessons_topic_position_key
    UNIQUE (tenant_id, topic_id, position) DEFERRABLE INITIALLY DEFERRED;

-- Publishing a course is a state transition worth being able to reconstruct.
-- published_at already exists; this index answers "what did this workspace
-- publish, most recent first" without a scan.
CREATE INDEX courses_tenant_published_idx
    ON courses (tenant_id, published_at DESC)
    WHERE status = 'published';

-- +goose Down
DROP INDEX courses_tenant_published_idx;
ALTER TABLE lessons DROP CONSTRAINT lessons_topic_position_key;
ALTER TABLE topics DROP CONSTRAINT topics_course_position_key;
ALTER TABLE lessons DROP COLUMN video_url, DROP COLUMN video_source, DROP COLUMN content;
