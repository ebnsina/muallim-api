# Performance

The competitive claim is that this is fast, so the guarantees are tested rather than hoped for.

- **No N+1.** A curriculum of any size loads in three queries. `database.Counter` counts queries under a context, and a test asserts the exact count across fixtures of growing size — replace the batched fetch with a loop and the build goes red.
- **Keyset pagination.** Measured on 50,000 courses, a keyset page reads 21 rows where the `OFFSET` equivalent reads 20,021. Cursors are opaque; there is no `COUNT(*)`; no list endpoint is unbounded.
- **Indexes cover filter and sort**, so plans are index scans with no sort node. Verify with `EXPLAIN (ANALYZE, COSTS OFF)` at realistic row counts.
- **Caching at both layers.** Tenant resolution is cached in process; read endpoints carry an `ETag` and answer `If-None-Match` with `304` and an empty body.
- **Bounded.** `statement_timeout` on every connection, a small pool, and a slow-query log that records the statement text — never its arguments.

Judge a page at the size it will be: `make seed-huge` builds ~1.1M rows across three workspaces.

Anything over ~200ms or touching a third party is a job, not a handler — grading, transcoding, email, transcription, analytics. Jobs run on River, which is Postgres-backed, so a job is enqueued in the transaction that produced it. There is no Redis.

## Measured, at the size it will be

`make seed-huge`, then the biggest workspace it builds: 1,204 members, 18,166
enrolments, 5,071 certificates. `EXPLAIN (ANALYZE, COSTS OFF)` with `app.tenant_id`
bound, because a plan taken with no tenant bound is a plan for a query row-level
security will not run.

| Query | Plan | Time |
|---|---|---|
| Members, first page | Index Scan, `memberships_tenant_joined_idx` | 0.79 ms |
| Members, the 22nd page (a keyset cursor 1,100 rows in) | Index Only Scan; the cursor is *in the index condition* | **0.19 ms** |
| Certificates a workspace has issued, newest first | Index Only Scan, `certificates_tenant_issued_idx` | 0.54 ms |
| An import resolving 500 addresses to members | one query; `lower(email)` is indexed | 14.9 ms |

The second row is the one that matters, and it is the whole argument for a keyset:
**a deep page is not slower than the first — it is faster**, because it seeks
straight to its position and reads exactly the fifty-one rows it wanted. `OFFSET
1100` would have read eleven hundred rows and thrown them away, and the last page of
a large school would have cost many times the first.

No Sort node and no Seq Scan on any of them. A plan measured on a table of forty-four
rows proves nothing — the planner ignores an index it does not need, so the only
honest place to check is a table big enough to make it want one.
