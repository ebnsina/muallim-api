-- +goose Up

-- The ID-card designer: a standalone visual-design tool for laying out a
-- student or staff ID card. It stores only a reusable template (a subject, a
-- canvas orientation, a palette, a background image, and a list of positioned
-- fields), never a card issued to a real person — the web renders a card for a
-- person by filling this template's fields.
--
-- `layout` is a JSON array of elements, each { id, kind, x, y, w, fontSize,
-- fontWeight, color, align, text } where kind is one of name, photo, id_number,
-- class_or_role, valid_until, blood_group, school_name, text and x/y/w are
-- fractions of the canvas in [0,1]. `photo` marks where the person's photo goes;
-- `text` and `school_name` carry static copy.

CREATE TABLE id_card_templates (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    name             text NOT NULL DEFAULT '',
    subject          text NOT NULL DEFAULT 'student'
                     CHECK (subject IN ('student', 'staff')),
    orientation      text NOT NULL DEFAULT 'portrait'
                     CHECK (orientation IN ('portrait', 'landscape')),
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
CREATE INDEX id_card_templates_tenant_idx
    ON id_card_templates (tenant_id, created_at DESC, id DESC);

ALTER TABLE id_card_templates ENABLE ROW LEVEL SECURITY;
ALTER TABLE id_card_templates FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON id_card_templates
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS id_card_templates;
