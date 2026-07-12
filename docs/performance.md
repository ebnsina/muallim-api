# Performance

The competitive claim is that this is fast, so the guarantees are tested rather than hoped for.

- **No N+1.** A curriculum of any size loads in three queries. `database.Counter` counts queries under a context, and a test asserts the exact count across fixtures of growing size — replace the batched fetch with a loop and the build goes red.
- **Keyset pagination.** Measured on 50,000 courses, a keyset page reads 21 rows where the `OFFSET` equivalent reads 20,021. Cursors are opaque; there is no `COUNT(*)`; no list endpoint is unbounded.
- **Indexes cover filter and sort**, so plans are index scans with no sort node. Verify with `EXPLAIN (ANALYZE, COSTS OFF)` at realistic row counts.
- **Caching at both layers.** Tenant resolution is cached in process; read endpoints carry an `ETag` and answer `If-None-Match` with `304` and an empty body.
- **Bounded.** `statement_timeout` on every connection, a small pool, and a slow-query log that records the statement text — never its arguments.

Judge a page at the size it will be: `make seed-huge` builds ~1.1M rows across three workspaces.

Anything over ~200ms or touching a third party is a job, not a handler — grading, transcoding, email, transcription, analytics. Jobs run on River, which is Postgres-backed, so a job is enqueued in the transaction that produced it. There is no Redis.
