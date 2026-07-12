-- +goose Up

-- "What do I owe, and when?" is a request-path query: it runs on every dashboard.
-- It walks assignments by deadline within a workspace, so the index carries the
-- filter and the sort together, and it is partial — an assignment with no due date
-- can never be an answer, and there is no reason to store it here.
CREATE INDEX assignments_tenant_due_idx
    ON assignments (tenant_id, due_at, id)
    WHERE due_at IS NOT NULL;

-- +goose Down
DROP INDEX assignments_tenant_due_idx;
