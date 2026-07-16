-- +goose Up

-- The institution library: the books a school holds, and the loans it makes to its
-- students. A book carries a copy count; a loan draws one copy while it is out and
-- returns it when the student brings the book back. Both are tenant-scoped and
-- backed by RLS, the same isolation the rest of the institution app follows.

CREATE TABLE library_books (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    title            text NOT NULL,
    author           text NOT NULL DEFAULT '',
    isbn             text,
    category         text,

    total_copies     int NOT NULL DEFAULT 1 CHECK (total_copies >= 0),
    -- Copies not currently out on loan. Never negative, never above the total: the
    -- issue and return statements guard both bounds.
    available_copies int NOT NULL DEFAULT 1
                     CHECK (available_copies >= 0 AND available_copies <= total_copies),

    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- The catalogue, newest first, and narrowed to a category — each keyset shape gets
-- its index so neither leaves a Sort node on the request path.
CREATE INDEX library_books_tenant_idx ON library_books (tenant_id, created_at DESC, id DESC);
CREATE INDEX library_books_category_idx ON library_books (tenant_id, category, created_at DESC, id DESC);

CREATE TABLE library_loans (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    book_id     uuid NOT NULL REFERENCES library_books (id) ON DELETE CASCADE,
    student_id  uuid NOT NULL REFERENCES students (id) ON DELETE CASCADE,

    borrowed_at timestamptz NOT NULL DEFAULT now(),
    due_at      timestamptz NOT NULL,
    returned_at timestamptz,

    status      text NOT NULL DEFAULT 'out' CHECK (status IN ('out', 'returned')),

    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- The loans board, newest first, filtered by status; and a single student's history.
CREATE INDEX library_loans_status_idx ON library_loans (tenant_id, status, created_at DESC, id DESC);
CREATE INDEX library_loans_student_idx ON library_loans (tenant_id, student_id, created_at DESC, id DESC);

ALTER TABLE library_books ENABLE ROW LEVEL SECURITY;
ALTER TABLE library_books FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON library_books
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE library_loans ENABLE ROW LEVEL SECURITY;
ALTER TABLE library_loans FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON library_loans
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS library_loans;
DROP TABLE IF EXISTS library_books;
