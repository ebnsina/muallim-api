-- +goose Up

-- Hifz: the daily Quran-memorization log a madrasa keeps for each student. Every
-- day a student recites their Sabaq (the new lesson), their Sabqi (recent lessons,
-- kept fresh), and their Manzil (the older portion, revised in a longer cycle).
-- One row is one of those recitations: a portion (a surah and an ayah range) and
-- how well it went. The Quran is 114 surahs; an ayah range is within one surah.
--
-- The frontier of what a student has memorized is a product question a madrasa
-- answers its own way, so it is not computed here — the log is the record, and a
-- summary reads the most recent Sabaq as the current position.

CREATE TABLE hifz_entries (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,

    student_id  uuid NOT NULL REFERENCES students (id) ON DELETE CASCADE,
    on_date     date NOT NULL,

    -- sabaq = new lesson, sabqi = near revision, manzil = far revision.
    kind        text NOT NULL CHECK (kind IN ('sabaq', 'sabqi', 'manzil')),

    surah       smallint NOT NULL CHECK (surah BETWEEN 1 AND 114),
    ayah_from   integer NOT NULL CHECK (ayah_from >= 1),
    ayah_to     integer NOT NULL CHECK (ayah_to >= ayah_from),

    -- How the recitation went. Adjustable per madrasa; these are the common four.
    rating      text NOT NULL DEFAULT 'good'
                CHECK (rating IN ('excellent', 'good', 'fair', 'weak')),
    note        text NOT NULL DEFAULT '',

    recorded_by uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- A student's log, newest first — the read path for both the daily view and the
-- current-position summary.
CREATE INDEX hifz_student_date_idx ON hifz_entries (tenant_id, student_id, on_date DESC, id DESC);

ALTER TABLE hifz_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE hifz_entries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON hifz_entries
    USING (tenant_id = app_current_tenant()) WITH CHECK (tenant_id = app_current_tenant());

-- +goose Down
DROP TABLE IF EXISTS hifz_entries;
