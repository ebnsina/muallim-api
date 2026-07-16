# Demo requests

Somebody on the marketing site asking to be shown the product. One table, one
endpoint, and a person reading the list.

It is the odd one out in this codebase and worth understanding as an exception
rather than a template. **The sender has no workspace** — that is the entire
content of the request — so `leads` is the only domain besides `tenant` that
touches a table with no `tenant_id` and no row-level security policy. Copying its
shape into a domain that *does* have a workspace would be a cross-tenant leak.

## Model

- **`demo_requests`** — `intent`, `name`, `email`, `phone`, `agreed_at`,
  `created_at`. No tenant column, no RLS policy, written through
  `database.WithoutTenant`.
- **`intent`** is a closed set — `creator`, `school`, `madrasa`, `coaching`,
  `agency`, `nonprofit`, `other` — enumerated by a `CHECK` **and** by the
  `Intents` catalogue in Go, the same way `automation`'s event set is. A free-text
  "who are you" column is a column nobody can count, and the point of asking is to
  tell a madrasa from an agency without reading two hundred rows by hand.
- **`agreed_at` is a timestamp, not a boolean.** The terms are agreed at the
  moment of asking and the row is the proof; `true` cannot be audited later.
- **`phone` is free text.** E.164 refuses half the ways a real person writes their
  own number, and Bangladesh is the primary market. We would rather hold it than
  validate it into the bin.

## The endpoint

`POST /v1/demo-requests` → `202`, `{ "received": true }`.

- **Unauthenticated, and exempt from tenant resolution** (`systemPaths` in
  `internal/httpapi/tenant.go`). It arrives on the marketing site's host, which
  resolves to no tenant; every other path answers "No workspace is configured for
  this address" there.
- **Rate-limited** per address per path. It verifies no credential and hashes
  nothing, but it is an unauthenticated `INSERT`: a shell and a loop is a table
  full of rubbish, and the person reading that list is our first impression.
- **The answer never varies by what we already hold.** No deduplication, no "you
  have already asked". Anyone may post here, so a response that differed for a
  known address would answer *"is this address in your database?"* for a stranger
  with a list. Duplicates are a human's problem to skim, and a cheaper one than the
  leak.
- **The response echoes nothing back.** There is nothing in the row the sender did
  not just type, and no id worth handing out. `no-store`, because a shared cache
  holding somebody's phone number is the whole of the problem.
- **An unticked box is a refusal** (`ErrNotAgreed`, 422), not a field error, and it
  is checked before the transaction opens: we do not hold somebody's phone number
  because they nearly agreed.

## The form

`muallim-web` renders it at `/demo`, and its radio options come from the generated
`RequestDemoRequest['intent']` union — the enum flows Go → OpenAPI → TypeScript, so
the set is written once. The labels are the web's own ("A madrasa" for `madrasa`):
the screens speak the reader's language, the contract keeps the domain's.

## Reading them

There is no admin screen yet. Today it is a query:

```sql
SELECT created_at, intent, name, phone, email
FROM demo_requests
ORDER BY created_at DESC;
```

That is what the `(created_at DESC, id DESC)` index is for, and it is the only way
this table is read.
