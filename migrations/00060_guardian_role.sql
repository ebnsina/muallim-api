-- +goose Up
-- The guardian role (a parent's portal login) postdates the role check written in
-- 00003/00005, so both the membership and the invitation must learn to allow it.
ALTER TABLE memberships DROP CONSTRAINT memberships_role_check;
ALTER TABLE memberships ADD CONSTRAINT memberships_role_check
	CHECK (role IN ('owner', 'admin', 'instructor', 'student', 'guardian'));

ALTER TABLE invitations DROP CONSTRAINT invitations_role_check;
ALTER TABLE invitations ADD CONSTRAINT invitations_role_check
	CHECK (role IN ('owner', 'admin', 'instructor', 'student', 'guardian'));

-- +goose Down
ALTER TABLE memberships DROP CONSTRAINT memberships_role_check;
ALTER TABLE memberships ADD CONSTRAINT memberships_role_check
	CHECK (role IN ('owner', 'admin', 'instructor', 'student'));

ALTER TABLE invitations DROP CONSTRAINT invitations_role_check;
ALTER TABLE invitations ADD CONSTRAINT invitations_role_check
	CHECK (role IN ('owner', 'admin', 'instructor', 'student'));
