-- +goose Up
-- A guardian record gains an optional account: the login through which a parent
-- sees their own child's day. Nullable — a guardian is a contact first, and only
-- some are ever given a portal login. ON DELETE SET NULL so removing the account
-- leaves the contact record intact.
ALTER TABLE guardians ADD COLUMN user_id uuid REFERENCES users (id) ON DELETE SET NULL;

-- The portal resolves a signed-in guardian to their wards through this column, so
-- it is the filter of a hot read. Partial: only linked guardians are ever matched.
CREATE INDEX guardians_user_id_idx ON guardians (tenant_id, user_id) WHERE user_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS guardians_user_id_idx;
ALTER TABLE guardians DROP COLUMN IF EXISTS user_id;
