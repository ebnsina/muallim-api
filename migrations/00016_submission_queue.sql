-- +goose Up

-- The marking queue: "which attempts at this quiz are waiting for me, oldest
-- first". A partial index, because the queue is the only thing that reads by
-- submitted_at, and an attempt that is graded has left it for ever.
--
-- Covering the filter and the sort together, so the plan is an index scan with no
-- sort node however many attempts a popular quiz has accumulated.
CREATE INDEX quiz_attempts_awaiting_review_idx
    ON quiz_attempts (tenant_id, quiz_id, submitted_at, id)
    WHERE status = 'awaiting_review';

-- And the same for an instructor reading the whole history of a quiz, which is
-- the other list they ask for.
CREATE INDEX quiz_attempts_tenant_quiz_submitted_idx
    ON quiz_attempts (tenant_id, quiz_id, submitted_at DESC, id DESC)
    WHERE submitted_at IS NOT NULL;

-- +goose Down
DROP INDEX quiz_attempts_tenant_quiz_submitted_idx;
DROP INDEX quiz_attempts_awaiting_review_idx;
