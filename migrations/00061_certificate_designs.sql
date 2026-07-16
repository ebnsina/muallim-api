-- +goose Up

-- The Certificate Designer: a standalone visual-design tool for laying out a
-- certificate. It is deliberately independent of the `certify` domain and its
-- issued certificates — this table stores only a reusable design (a canvas
-- orientation, a palette, a background image, and a list of positioned elements),
-- never a certificate awarded to anybody.
--
-- `layout` is a JSON array of elements, each { id, kind, x, y, w, fontSize,
-- fontWeight, color, align, text } where kind is one of title, learner, course,
-- date, serial, signatory, text and x/y/w are fractions of the canvas in [0,1].

CREATE TABLE certificate_designs (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    name             text NOT NULL DEFAULT '',
    orientation      text NOT NULL DEFAULT 'landscape'
                     CHECK (orientation IN ('landscape', 'portrait')),
    accent           text NOT NULL DEFAULT '',
    background_color  text NOT NULL DEFAULT '',
    -- The object-store key of an uploaded background image; null is no background.
    background_key   text,
    layout           jsonb NOT NULL DEFAULT '[]',

    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- The keyset listing, newest first: covers the tenant filter and the (created_at,
-- id) sort together, so the cursor is a real keyset and not an OFFSET in disguise.
CREATE INDEX certificate_designs_tenant_idx
    ON certificate_designs (tenant_id, created_at DESC, id DESC);

ALTER TABLE certificate_designs ENABLE ROW LEVEL SECURITY;
ALTER TABLE certificate_designs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON certificate_designs
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS certificate_designs;
