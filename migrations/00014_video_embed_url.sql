-- +goose Up

-- The URL of the player, as opposed to the URL the author typed.
--
-- `video_url` is an author's input: a YouTube watch link, a Vimeo page, a
-- Cloudflare Stream id. Until now it went straight into an `iframe` src, which
-- made anybody holding `course:write` able to run a page of their choosing on the
-- workspace's own origin, for every reader of the lesson. It also did not work:
-- a YouTube *watch* URL does not play in a frame.
--
-- So the two are now different things. `video_url` stays what the author sees and
-- edits; `video_embed_url` is written by a provider driver from a validated id,
-- and is the only one a template may render. There is no constraint here that can
-- enforce that — the check lives in `internal/media`, because "is this URL safe to
-- frame" is not a statement about a string's shape.
ALTER TABLE lessons
    ADD COLUMN video_embed_url text NOT NULL DEFAULT '';

-- Existing rows keep their author input and get no player. A lesson whose video
-- has never been re-saved renders without one, which is the safe reading of a URL
-- nothing has vouched for. Editing the lesson resolves it.

-- +goose Down
ALTER TABLE lessons DROP COLUMN video_embed_url;
