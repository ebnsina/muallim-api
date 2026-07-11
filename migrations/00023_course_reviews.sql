-- +goose Up

-- A learner's rating and written review of a course.
--
-- One per learner per course: a review is a considered verdict, not a running
-- commentary, and the unique index lets a second submission edit the first
-- rather than stack a wall of duplicates. Left in the enroll domain because the
-- right to review is the fact enroll owns — you review a course you enrolled in.
--
-- The rating is constrained to 1..5 in the column, not just the service, because
-- a star count outside that range is meaningless however it arrives.
--
-- The author is remembered but may go: `ON DELETE SET NULL` keeps the review on
-- the course when the account is erased, the same way an announcement outlives
-- the instructor who pinned it.
CREATE TABLE course_reviews (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    course_id uuid NOT NULL REFERENCES courses (id) ON DELETE CASCADE,
    user_id   uuid REFERENCES users (id) ON DELETE SET NULL,

    rating smallint NOT NULL CHECK (rating BETWEEN 1 AND 5),
    body   text NOT NULL DEFAULT '',

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- One review per (course, learner). A returning reviewer edits their verdict.
CREATE UNIQUE INDEX course_reviews_one_per_learner_idx
    ON course_reviews (tenant_id, course_id, user_id);

-- A course's reviews, newest first — the order a review wall is read, and the
-- filter (tenant, course) the average is computed over.
CREATE INDEX course_reviews_course_idx
    ON course_reviews (tenant_id, course_id, created_at DESC, id);

-- Row-level security. ENABLE alone exempts the table owner, which is the role the
-- application connects as; FORCE subjects the owner too. Tenant isolation is the
-- net; that a learner writes only their own review is the service's job.
ALTER TABLE course_reviews ENABLE ROW LEVEL SECURITY;
ALTER TABLE course_reviews FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON course_reviews
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE course_reviews;
