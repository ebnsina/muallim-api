-- +goose Up

-- A demo request is somebody asking to be shown the product. It arrives before
-- there is a workspace to belong to — the whole point is that they have not signed
-- up — so, like `tenants`, this table is not tenant-scoped and carries no row-level
-- security policy. It is written through database.WithoutTenant.
--
-- Nothing here identifies a member: an address in this table proves only that
-- somebody typed it into a public form. It is never joined to users, and answering
-- a request is a human reading a list, not the system granting anything.
CREATE TABLE demo_requests (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Who is asking. Closed set, and the same set the Go catalogue in
    -- internal/leads enumerates: a value the code cannot name is a row nobody can
    -- read back, and a form nobody can fill.
    intent     text        NOT NULL
               CHECK (intent IN ('creator', 'school', 'madrasa', 'coaching',
                                 'agency', 'nonprofit', 'other')),

    name       text        NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
    email      text        NOT NULL CHECK (length(btrim(email)) BETWEEN 3 AND 320),
    -- Bangladesh first: a mobile number is how most of this market actually replies.
    -- Free text, because a phone number is not a type — E.164 refuses half the ways
    -- a real person writes their own number, and we would rather have it than
    -- validate it into the bin.
    phone      text        NOT NULL CHECK (length(btrim(phone)) BETWEEN 3 AND 40),

    -- The terms are agreed at the moment of asking, and the proof is the row.
    -- Storing *when* rather than a boolean: "true" cannot be audited later.
    agreed_at  timestamptz NOT NULL,

    created_at timestamptz NOT NULL DEFAULT now()
);

-- The list is read newest first, and that is the only way it is read.
CREATE INDEX demo_requests_created_at_idx ON demo_requests (created_at DESC, id DESC);

-- +goose Down
DROP TABLE demo_requests;
