-- +goose Up

-- Real-time messaging. A conversation is one of three kinds: a `course` channel
-- (one per course, everyone who may take it belongs), a `direct` 1:1 between two
-- members, or an ad-hoc `group`. Messages are plain text; membership is explicit
-- in chat_members, and every read is guarded by "are you a member". This layer is
-- persistence + REST only; the realtime (WebSocket) fan-out is added separately.
--
-- Access is a membership check, never a request parameter: a non-member reading a
-- conversation gets 403, and a conversation that is not theirs is simply absent
-- from their list.

CREATE TABLE chat_conversations (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    kind            text NOT NULL CHECK (kind IN ('course', 'direct', 'group')),
    -- Set only for a course channel; the channel dies with its course.
    course_id       uuid REFERENCES courses (id) ON DELETE CASCADE,
    title           text NOT NULL DEFAULT '',
    created_by      uuid REFERENCES users (id) ON DELETE SET NULL,

    -- The canonical pair for a direct conversation, low uuid then high, so a pair
    -- has exactly one row regardless of who opened it. Null for course and group.
    dm_low          uuid,
    dm_high         uuid,

    -- Bumped on every message so a member's conversation list keysets by recency.
    last_message_at timestamptz NOT NULL DEFAULT now(),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- A course has exactly one channel.
CREATE UNIQUE INDEX chat_conversations_course_key
    ON chat_conversations (tenant_id, course_id) WHERE kind = 'course';
-- A pair of members has exactly one direct conversation.
CREATE UNIQUE INDEX chat_conversations_direct_key
    ON chat_conversations (tenant_id, dm_low, dm_high) WHERE kind = 'direct';

CREATE TABLE chat_members (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    conversation_id uuid NOT NULL REFERENCES chat_conversations (id) ON DELETE CASCADE,
    user_id         uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role            text NOT NULL DEFAULT 'member' CHECK (role IN ('member', 'admin')),
    -- The high-water mark for unread counting; null means nothing read yet.
    last_read_at    timestamptz,

    created_at      timestamptz NOT NULL DEFAULT now()
);

-- A user belongs to a conversation at most once.
CREATE UNIQUE INDEX chat_members_unique
    ON chat_members (tenant_id, conversation_id, user_id);
-- List a user's conversations.
CREATE INDEX chat_members_user_idx ON chat_members (tenant_id, user_id);

CREATE TABLE chat_messages (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    conversation_id uuid NOT NULL REFERENCES chat_conversations (id) ON DELETE CASCADE,
    sender_id       uuid REFERENCES users (id) ON DELETE SET NULL,
    body            text NOT NULL,

    created_at      timestamptz NOT NULL DEFAULT now()
);

-- Keyset a conversation's messages, newest first.
CREATE INDEX chat_messages_keyset_idx
    ON chat_messages (tenant_id, conversation_id, created_at DESC, id DESC);

ALTER TABLE chat_conversations ENABLE ROW LEVEL SECURITY;
ALTER TABLE chat_conversations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON chat_conversations
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE chat_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE chat_members FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON chat_members
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE chat_messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE chat_messages FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON chat_messages
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS chat_messages;
DROP TABLE IF EXISTS chat_members;
DROP TABLE IF EXISTS chat_conversations;
