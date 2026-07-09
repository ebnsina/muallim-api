-- +goose Up

-- Authors need to find the courses they have not published yet, which is the one
-- listing that does not filter on status.
--
-- courses_tenant_status_created_idx cannot serve it: its leading columns are
-- (tenant_id, status), so a query with no status predicate can seek on tenant_id
-- and then must sort. This index covers the filter and the sort together, so the
-- plan is an index scan with no Sort node, and the authoring dashboard of a
-- workspace with forty thousand courses costs what one with forty costs.
CREATE INDEX courses_tenant_created_idx
    ON courses (tenant_id, created_at DESC, id DESC);

-- +goose Down
DROP INDEX courses_tenant_created_idx;
