-- +goose Up

-- A course's preview: the clip a stranger watches before deciding to enrol.
--
-- Three columns, exactly as a lesson's video is three columns, and for the same
-- reason. `preview_source` says who serves it; `preview_url` is what the author
-- typed and is shown back to them; `preview_embed_url` is written by a provider
-- driver from a validated id and is the only one of the three that may ever reach
-- an `iframe` src. An embed URL a request body could set is a request body that
-- runs on this origin.
ALTER TABLE courses
    ADD COLUMN preview_source text NOT NULL DEFAULT 'none'
        CHECK (preview_source IN ('none', 'youtube', 'vimeo', 'embed', 'hosted')),
    ADD COLUMN preview_url text NOT NULL DEFAULT '',
    ADD COLUMN preview_embed_url text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE courses
    DROP COLUMN preview_source,
    DROP COLUMN preview_url,
    DROP COLUMN preview_embed_url;
