# Accounting ledger

A school's own books: the income and expense heads a workspace defines, and the
dated amounts posted against them. A modular-monolith domain like the rest — it
knows nothing about HTTP, references categories by id, and is tenant-scoped with RLS
behind every table. Money is `bigint` minor units + `currency char(3)`, defaulting
to BDT poisha, never a float.

This is the institution's own accounting. Nothing here moves money; it records money
that already moved. It is separate from `fees` (the school billing its students) and
from `commerce` (a learner buying a course).

## Model

- **`ledger_categories`** — a named income or expense head. `name` and a `kind` of
  `income` or `expense` (a `CHECK`). Listed by kind then name.
- **`ledger_entries`** — one dated amount posted against a category. A `category_id`
  (FK, `ON DELETE CASCADE`), `amount` (`>= 0`), a `currency`, an `occurred_on` date,
  and an optional `description`. A bad `category_id` surfaces as `ErrNotFound`, not a
  500.

The listing is keyset-paginated, newest first (`occurred_on DESC, id DESC`); the
by-category shape rides its own covering index `(tenant_id, category_id, occurred_on
DESC, id DESC)`, and the unfiltered board rides `(tenant_id, occurred_on DESC, id
DESC)` — no `Sort` node on the request path, no `OFFSET`. Kind narrows through the
parent category; a date range filters on `occurred_on`.

The summary totals income and expense per currency in one grouped query (a `JOIN` to
the category for its kind, `GROUP BY kind, currency`) and computes net (`income −
expense`) per currency in Go — no query in a loop.

## Endpoints

All under `academics:manage`, admin-only, bearer auth.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v1/ledger/categories` | The workspace's income and expense heads, by kind then name. |
| `POST` | `/v1/ledger/categories` | Define a head. Body: `name`, `kind` (`income`\|`expense`). |
| `GET` | `/v1/ledger/entries` | Entries, newest first. Filter by `kind`, `category_id`, `from`, `to`. Keyset `cursor` + `has_more`. |
| `POST` | `/v1/ledger/entries` | Record an entry. Body: `category_id`, `amount`, `currency?`, `occurred_on`, `description?`. |
| `GET` | `/v1/ledger/summary` | Income/expense/net totals per currency. Same filters as the listing. |

## Errors

| Sentinel | Status | When |
| --- | --- | --- |
| `ErrNotFound` | 404 | No such entry, or an entry against a category that does not exist in this workspace. |
| `ErrInvalidCategory` | 422 | An unnamed category, or an unknown kind. |
| `ErrInvalidEntry` | 422 | A negative amount, a missing date, or a malformed currency. |
| `ErrInvalidPage` | 422 | The page cursor did not decode. |
