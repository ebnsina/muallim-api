# Library

The institution's library: the books a school holds and the loans it makes to its
students. A modular-monolith domain like the rest — it knows nothing about HTTP,
references students by id, and is tenant-scoped with RLS behind every table.

## Model

- **`library_books`** — a title the school holds. `title`, `author`, optional
  `isbn` and `category`, and two counts: `total_copies` and `available_copies`.
  A check constraint keeps `0 <= available_copies <= total_copies`.
- **`library_loans`** — one copy lent to one student. `book_id`, `student_id`,
  `borrowed_at`, `due_at`, nullable `returned_at`, and a `status` of `out` or
  `returned`.

Issuing a loan draws one copy (`available_copies - 1`, guarded `> 0` in the same
statement, so the last copy can never be lent twice). Returning a loan puts the
copy back (`LEAST(available_copies + 1, total_copies)`), guarded by
`status = 'out'` so a repeat return is a no-op.

Both listings are keyset-paginated, newest first (`created_at DESC, id DESC`), and
each filter shape has an index that covers its sort — no `Sort` node on the request
path, no `OFFSET`.

## Endpoints

All under `academics:manage`, admin-only, bearer auth.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v1/library/books` | Catalogue, newest first. Filter by `category`. Keyset `cursor` + `has_more`. |
| `POST` | `/v1/library/books` | Add a book. Body: `title`, `author?`, `isbn?`, `category?`, `total_copies?` (defaults to 1). |
| `GET` | `/v1/library/loans` | Loans, newest first. Filter by `student_id` and/or `status`. Keyset `cursor` + `has_more`. |
| `POST` | `/v1/library/loans` | Lend a book. Body: `book_id`, `student_id`, `due_at?` (RFC 3339; defaults to 14 days out). |
| `POST` | `/v1/library/loans/{id}/return` | Return a borrowed book and restore its copy. |

## Errors

| Sentinel | Status | When |
| --- | --- | --- |
| `ErrNotFound` | 404 | No such book or loan in this workspace. |
| `ErrNoCopies` | 409 | Every copy of the book is already out. |
| `ErrAlreadyReturned` | 409 | The loan was returned already. |
| `ErrInvalidBook` | 422 | Missing title, or an impossible copy count. |
| `ErrInvalidLoan` | 422 | Missing book/student, or a due date too far off. |
| `ErrInvalidPage` | 422 | The page cursor did not decode. |
