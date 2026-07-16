# Chat

Real-time messaging for a workspace. Three kinds of conversation — a **course
channel**, a **direct** 1:1, and an ad-hoc **group** — over one persistent model.
A modular-monolith domain like the rest: it knows nothing about HTTP, references
courses and users by id, is tenant-scoped with RLS behind every table, and does not
import a sibling — the enrolment check a course channel needs is passed in by
`internal/httpapi`, which holds the `enroll.Service`.

This module is the **persistent + REST foundation only**. The realtime layer
(WebSocket fan-out) is added separately by the coordinator; it drives this service
through `Send`, `IsMember`, `EnsureMember`, `Conversation`, and `ListForUser`.

## Model

- **`chat_conversations`** — one thread of messages. `kind` is `course`, `direct`,
  or `group`. A `course_id` (FK, `ON DELETE CASCADE`) is set only for a course
  channel; `title` for a group; `created_by` (FK, `ON DELETE SET NULL`). A partial
  unique index `(tenant_id, course_id) WHERE kind='course'` gives a course exactly
  **one** channel. A direct conversation carries its members' ids as a canonical
  pair `(dm_low, dm_high)` with a partial unique index `WHERE kind='direct'`, so the
  same two people always resolve to one conversation whichever way round they are
  named. `last_message_at` is bumped on every send and is what the conversation list
  keysets by.
- **`chat_members`** — who belongs, with a `role` of `member` or `admin` and a
  nullable `last_read_at` high-water mark for unread counting. Unique
  `(tenant_id, conversation_id, user_id)`; indexed `(tenant_id, user_id)` to list a
  user's conversations.
- **`chat_messages`** — a message with a `sender_id` (FK, `ON DELETE SET NULL` — the
  row outlives the account) and a `body`. Keyset index
  `(tenant_id, conversation_id, created_at DESC, id DESC)`.

## The three kinds

- **Direct** — `EnsureDirect(a, b)` is find-or-create: the pair is canonical-ordered
  so one conversation exists per pair, idempotently, regardless of argument order.
  Both parties are added as members. A direct conversation with oneself is refused.
- **Group** — `CreateGroup(title, member_ids, creator)` opens an ad-hoc group. The
  creator joins as **admin**; the named members join as `member`. Admins alone add
  and remove members.
- **Course channel** — `EnsureCourseChannel(course_id)` gets or creates the course's
  single channel. A learner joins it through `EnsureMember` once their enrolment is
  confirmed — the channel does not auto-enrol anyone.

## The access rule

Access is **membership**, checked in SQL, never a request parameter:

- Every read (`GET` a conversation, its messages) and `Send` is guarded by
  `IsMember`. A non-member gets **403** on a conversation they may be told exists,
  and **404** on one they may not — the same not-found-hides-existence rule the rest
  of the codebase follows.
- `ListForUser` returns only the caller's own conversations, most recently active
  first, each with its **last message** and the caller's **unread count**, computed
  in one query with two lateral joins — no query per conversation.
- Adding or removing a group member requires the caller to be a group **admin**.
- Joining a course channel (`POST /v1/courses/{slug}/chat/channel`) requires a live
  enrolment (`enroll.IsEnrolled`) **or** `course:read`+`course:write` on the course.

All chat responses are `private, no-store`: what a caller sees depends on which
conversations they belong to, so nothing is shared-cacheable.

Every list — messages and conversations alike — is keyset-paginated: fetch
`limit + 1`, opaque base64 cursor on the wire (`next_cursor`), no `OFFSET`, no
`COUNT(*)`.

## Endpoints

All gated on `course:read` (any authenticated member); each read/send additionally
checks membership as above.

- `GET  /v1/chat/conversations` — the caller's conversations (keyset).
- `POST /v1/chat/conversations` — `{kind:'direct', user_id}` opens/reuses the 1:1;
  `{kind:'group', title, member_ids}` opens a group.
- `GET  /v1/chat/conversations/{id}` — a conversation and its members (member only).
- `GET  /v1/chat/conversations/{id}/messages` — messages, newest first, keyset
  (member only).
- `POST /v1/chat/conversations/{id}/messages` — send a message (member only).
- `POST /v1/chat/conversations/{id}/read` — mark read up to now.
- `POST /v1/chat/conversations/{id}/members` — add someone (group admin only).
- `DELETE /v1/chat/conversations/{id}/members/{user_id}` — remove someone (group
  admin only).
- `POST /v1/courses/{slug}/chat/channel` — join (or open) a course's channel; how an
  enrolled learner joins.

## Errors

Domain sentinels, mapped to status by `internal/httpapi` and nothing else:

- `ErrNotFound` → **404** — no such conversation, or one hidden from the caller.
- `ErrNotMember` → **403** — a read or send against a conversation the caller is not
  in.
- `ErrInvalid` → **422** — an empty body, a group with no members, a self-direct.
- `ErrInvalidPage` → **422** — an opaque cursor that did not decode.

## Realtime (added separately)

The WebSocket layer is **not** in this module. When it lands it reuses this
service's `Send` (so a socket message and a persisted row are the same write),
`IsMember` (the subscribe gate), and `ListForUser`/`Conversation` for the initial
snapshot. Persistence and REST here are the source of truth; the socket is a
delivery channel over them.
