-- +goose Up

-- A course's thumbnail: the picture a catalogue card and the course page show
-- instead of the gradient placeholder. `image_key` names an object in the store,
-- never a public URL — the bytes are private and served through a signed,
-- short-lived redirect, so an unpublished course's image is no more reachable than
-- the course. Empty means no image, which is every course until an author sets one.
ALTER TABLE courses ADD COLUMN image_key text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE courses DROP COLUMN image_key;
