-- +goose Up

-- Content drip releases a course's lessons over time instead of all at once.
--
-- The mode belongs to the course, because it decides how the per-lesson columns
-- below are read. Storing it per lesson would let one course hold lessons that
-- disagree about what "available" means, and nothing could resolve them.
--
-- The four modes the product promises are three columns here. "Release after the
-- prerequisites are finished" is not one of them: course prerequisites already
-- gate enrolment, and a learner who is not enrolled reads no lesson at all.
ALTER TABLE courses
    ADD COLUMN drip_mode text NOT NULL DEFAULT 'none'
        CHECK (drip_mode IN ('none', 'scheduled', 'after_enrolment', 'sequential'));

-- Read only when the course is in 'scheduled' mode: the wall-clock instant a
-- lesson opens, the same for every learner.
ALTER TABLE lessons
    ADD COLUMN available_at timestamptz;

-- Read only when the course is in 'after_enrolment' mode: how long after each
-- learner's own enrolment the lesson opens. Different dates for different people,
-- which is why it cannot be a timestamp.
ALTER TABLE lessons
    ADD COLUMN available_after_days integer
        CHECK (available_after_days IS NULL OR available_after_days >= 0);

-- 'sequential' mode needs no column. A lesson is open when every lesson before it
-- in the curriculum is complete, and the curriculum already knows its own order.

-- +goose Down
ALTER TABLE lessons DROP COLUMN available_after_days, DROP COLUMN available_at;
ALTER TABLE courses DROP COLUMN drip_mode;
