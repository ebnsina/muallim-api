-- +goose Up

-- Points a learner has earned, one row per earning event. Append-only: the unique
-- key (tenant, user, reason, source) is what makes an award idempotent — finishing
-- the same lesson twice, or a retried transaction, earns nothing new, so points
-- cannot be farmed by toggling a lesson complete and back.
CREATE TABLE point_awards (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- What was done ('lesson', 'course'), and the thing it was done to.
    reason    text NOT NULL,
    source_id uuid NOT NULL,

    points integer NOT NULL CHECK (points >= 0),

    created_at timestamptz NOT NULL DEFAULT now(),

    UNIQUE (tenant_id, user_id, reason, source_id)
);

CREATE INDEX point_awards_user_idx ON point_awards (tenant_id, user_id, reason);

-- A learner's running total, a roll-up of point_awards kept in step by the
-- transaction that adds an award — never summed on read, so a leaderboard is an
-- ordered index scan, not a group-by over every award ever made.
CREATE TABLE user_points (
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    points integer NOT NULL DEFAULT 0 CHECK (points >= 0),

    updated_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, user_id)
);

-- The leaderboard reads this: highest first, id to break ties. An index scan, no
-- sort node.
CREATE INDEX user_points_leaderboard_idx ON user_points (tenant_id, points DESC, user_id);

-- Badges a learner has earned. The badge itself is a code-defined string — the
-- catalog is a fixed set, like the question types — so this table only records who
-- has which. The primary key makes a re-award a no-op.
CREATE TABLE user_badges (
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    badge text NOT NULL,

    awarded_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, user_id, badge)
);

CREATE INDEX user_badges_user_idx ON user_badges (tenant_id, user_id, awarded_at DESC);

-- Row-level security. ENABLE alone exempts the table owner, the role the
-- application connects as; FORCE subjects the owner too. Tenant isolation only; a
-- leaderboard is shared among a workspace's members by design.
ALTER TABLE point_awards ENABLE ROW LEVEL SECURITY;
ALTER TABLE point_awards FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON point_awards
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE user_points ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_points FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON user_points
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE user_badges ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_badges FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON user_badges
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE user_badges;
DROP TABLE user_points;
DROP TABLE point_awards;
