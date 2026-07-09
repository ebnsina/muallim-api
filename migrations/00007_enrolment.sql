-- +goose Up

-- An enrolment is a person's right to study a course.
--
-- It is not a payment. Commerce will create enrolments; so will a manual grant by
-- an instructor, a bulk CSV import, and a free course. `source` records which,
-- because "why does this student have access" is the first question support asks.
CREATE TABLE enrolments (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    course_id  uuid        NOT NULL REFERENCES courses (id) ON DELETE CASCADE,
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    status     text        NOT NULL DEFAULT 'active'
               CHECK (status IN ('active', 'completed', 'expired', 'cancelled')),
    source     text        NOT NULL DEFAULT 'self'
               CHECK (source IN ('self', 'granted', 'purchase', 'import')),

    -- Null means it never expires. A cohort or a subscription sets one.
    expires_at   timestamptz,
    enrolled_at  timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- One enrolment per person per course. Re-enrolling reactivates the row rather
-- than creating a second, so progress survives.
CREATE UNIQUE INDEX enrolments_course_user_key ON enrolments (tenant_id, course_id, user_id);

-- "What am I studying", newest first. Covers the filter and the sort.
CREATE INDEX enrolments_user_enrolled_idx ON enrolments (tenant_id, user_id, enrolled_at DESC, id DESC);

-- The access check runs on every lesson read, so it must be an index lookup.
CREATE INDEX enrolments_lookup_idx ON enrolments (tenant_id, user_id, course_id)
    WHERE status = 'active';

-- Per-lesson progress. course_id is denormalised so recomputing a course's
-- progress does not need to join back through topics.
CREATE TABLE lesson_progress (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    lesson_id uuid NOT NULL REFERENCES lessons (id) ON DELETE CASCADE,
    course_id uuid NOT NULL REFERENCES courses (id) ON DELETE CASCADE,

    completed_at    timestamptz,
    seconds_watched integer     NOT NULL DEFAULT 0 CHECK (seconds_watched >= 0),
    last_seen_at    timestamptz NOT NULL DEFAULT now(),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX lesson_progress_user_lesson_key ON lesson_progress (tenant_id, user_id, lesson_id);
CREATE INDEX lesson_progress_course_idx ON lesson_progress (tenant_id, user_id, course_id);

-- Course progress is a materialised roll-up, recomputed in the transaction that
-- changes a lesson's state.
--
-- Not computed on read: a course page would then count every lesson and every
-- completion of every student on every request. Not a trigger either — a trigger
-- is action at a distance, and this recompute needs to be visible in the code
-- that causes it.
CREATE TABLE course_progress (
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    course_id uuid NOT NULL REFERENCES courses (id) ON DELETE CASCADE,

    lessons_completed integer     NOT NULL DEFAULT 0 CHECK (lessons_completed >= 0),
    lessons_total     integer     NOT NULL DEFAULT 0 CHECK (lessons_total >= 0),
    percent           smallint    NOT NULL DEFAULT 0 CHECK (percent BETWEEN 0 AND 100),
    updated_at        timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, user_id, course_id),
    CONSTRAINT course_progress_within_bounds CHECK (lessons_completed <= lessons_total)
);

-- +goose StatementBegin
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['enrolments', 'lesson_progress', 'course_progress'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format(
            'CREATE POLICY tenant_isolation ON %I
                 USING (tenant_id = app_current_tenant())
                 WITH CHECK (tenant_id = app_current_tenant())', t);
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE course_progress;
DROP TABLE lesson_progress;
DROP TABLE enrolments;
