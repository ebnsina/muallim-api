-- +goose Up

-- The forum: community discussion, broader than a lesson's Q&A.
--
-- A space is a board. It is either workspace-wide (course_id NULL — the whole
-- community) or bound to a course (visible to whoever may take it). Threads live
-- in a space; posts are replies to a thread. Unlike Q&A, which is per-lesson and
-- private to course access, this is where a school's community talks.
CREATE TABLE forum_spaces (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    -- NULL means workspace-wide; otherwise the course whose members may see it.
    course_id uuid REFERENCES courses (id) ON DELETE CASCADE,

    title       text NOT NULL,
    description text NOT NULL DEFAULT '',

    -- Dense ordering for the board list; a moderator arranges the spaces.
    position integer NOT NULL DEFAULT 0,

    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX forum_spaces_tenant_idx ON forum_spaces (tenant_id, position, id);

-- A thread is a titled discussion with an opening body. The opening post is the
-- thread itself; forum_posts are the replies. reply_count and last_activity_at
-- are a roll-up kept in step by the transaction that adds a reply — never counted
-- on read, so a busy board does not count every post for every visitor.
CREATE TABLE forum_threads (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    space_id  uuid NOT NULL REFERENCES forum_spaces (id) ON DELETE CASCADE,
    author_id uuid REFERENCES users (id) ON DELETE SET NULL,

    title text NOT NULL,
    body  text NOT NULL,

    -- A pinned thread sits above the rest; a locked one takes no new replies.
    pinned boolean NOT NULL DEFAULT false,
    locked boolean NOT NULL DEFAULT false,

    reply_count      integer     NOT NULL DEFAULT 0 CHECK (reply_count >= 0),
    last_activity_at timestamptz NOT NULL DEFAULT now(),

    created_at timestamptz NOT NULL DEFAULT now()
);

-- A space's threads: pinned first, then most recently active. The keyset the
-- thread list pages over, in one index seek.
CREATE INDEX forum_threads_space_idx
    ON forum_threads (tenant_id, space_id, pinned DESC, last_activity_at DESC, id DESC);

CREATE TABLE forum_posts (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    thread_id uuid NOT NULL REFERENCES forum_threads (id) ON DELETE CASCADE,
    author_id uuid REFERENCES users (id) ON DELETE SET NULL,

    body text NOT NULL,

    created_at timestamptz NOT NULL DEFAULT now()
);

-- A thread's replies, oldest first — the order a conversation is read, and the
-- keyset the post list pages over.
CREATE INDEX forum_posts_thread_idx
    ON forum_posts (tenant_id, thread_id, created_at, id);

-- Row-level security. ENABLE alone exempts the table owner, the role the
-- application connects as; FORCE subjects the owner too. Tenant isolation is the
-- net; who within a tenant may see a space is the service's decision, from the
-- space's course binding and the caller's enrolment.
ALTER TABLE forum_spaces ENABLE ROW LEVEL SECURITY;
ALTER TABLE forum_spaces FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON forum_spaces
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE forum_threads ENABLE ROW LEVEL SECURITY;
ALTER TABLE forum_threads FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON forum_threads
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE forum_posts ENABLE ROW LEVEL SECURITY;
ALTER TABLE forum_posts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON forum_posts
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE forum_posts;
DROP TABLE forum_threads;
DROP TABLE forum_spaces;
