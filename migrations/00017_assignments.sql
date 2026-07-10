-- +goose Up

-- An assignment is work a person hands in. It hangs off a lesson, exactly as a
-- quiz does, so every rule the curriculum already enforces — drafts invisible,
-- drip holding a lesson back, a preview readable without enrolling — governs it
-- for free and none of it is restated here.
CREATE TABLE assignments (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    lesson_id uuid NOT NULL REFERENCES lessons (id) ON DELETE CASCADE,

    title        text NOT NULL,
    instructions text NOT NULL DEFAULT '',

    points         integer NOT NULL DEFAULT 100 CHECK (points >= 0),
    passing_points integer NOT NULL DEFAULT 0 CHECK (passing_points >= 0),

    -- The upload policy. Enforced when the URL is signed, and by the object store
    -- when the bytes arrive: `max_bytes` is signed into the upload, so a client
    -- that wants to send more has to break SigV4 to do it.
    max_files integer NOT NULL DEFAULT 3 CHECK (max_files BETWEEN 1 AND 20),
    max_bytes bigint  NOT NULL DEFAULT 26214400 CHECK (max_bytes BETWEEN 1 AND 1073741824),

    due_at     timestamptz,
    allow_late boolean NOT NULL DEFAULT true,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    CHECK (passing_points <= points)
);

CREATE UNIQUE INDEX assignments_tenant_lesson_key ON assignments (tenant_id, lesson_id);

-- One submission per learner per assignment.
--
-- Not one per attempt. A quiz is a thing you sit; an assignment is a thing you
-- hand in, and handing it in twice is handing in a revision of the same work.
-- Until it is submitted the learner adds and removes files at will.
CREATE TABLE assignment_submissions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    assignment_id uuid NOT NULL REFERENCES assignments (id) ON DELETE CASCADE,
    user_id       uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    status text NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft', 'submitted', 'graded')),

    submitted_at timestamptz,
    graded_at    timestamptz,

    -- NULL until a person has marked it. A grade is not a thing to guess at.
    points   integer CHECK (points IS NULL OR points >= 0),
    feedback text NOT NULL DEFAULT '',

    -- Who marked it. An anonymous grade is a grade nobody can be asked about.
    graded_by uuid REFERENCES users (id) ON DELETE SET NULL,

    -- True when it arrived after the due date. Recorded at submission rather than
    -- computed on read: moving the deadline afterwards must not retroactively make
    -- somebody late, or on time.
    late boolean NOT NULL DEFAULT false,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX assignment_submissions_one_per_learner_key
    ON assignment_submissions (tenant_id, assignment_id, user_id);

-- "What is waiting for me to mark", oldest first. A partial index, because a
-- graded submission has left that queue for ever.
CREATE INDEX assignment_submissions_awaiting_idx
    ON assignment_submissions (tenant_id, assignment_id, submitted_at, id)
    WHERE status = 'submitted';

-- A file that has been uploaded and confirmed.
--
-- No row exists until the bytes are in the object store and this system has asked
-- it how many there are. A row written when the URL was signed would be a row
-- pointing at nothing whenever an upload was abandoned — which is most of the
-- time, on a phone, on a train.
CREATE TABLE submission_files (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    submission_id uuid NOT NULL REFERENCES assignment_submissions (id) ON DELETE CASCADE,

    -- The object's key. Unique across the whole installation, not merely the
    -- tenant: two rows pointing at one object is one deletion away from a
    -- dangling link.
    object_key text NOT NULL,

    -- The name the learner's computer gave it. Shown, and used as the download
    -- name; never used to build the key, which is a uuid, so no filename this
    -- system stores can name a path.
    filename     text NOT NULL,
    content_type text NOT NULL DEFAULT '',
    bytes        bigint NOT NULL CHECK (bytes > 0),

    uploaded_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX submission_files_object_key_key ON submission_files (object_key);
CREATE INDEX submission_files_tenant_submission_idx
    ON submission_files (tenant_id, submission_id, uploaded_at, id);

-- Row-level security. ENABLE alone exempts the table owner, which is the role the
-- application connects as; FORCE subjects the owner too.
--
-- As with quiz attempts, these policies isolate tenants and nothing else. That a
-- learner may not read another learner's submission is not a tenancy question,
-- and a policy that tried to answer it would need the current user, which the
-- transaction does not carry. Ownership is enforced in the service.
-- +goose StatementBegin
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['assignments', 'assignment_submissions', 'submission_files'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format(
            'CREATE POLICY tenant_isolation ON %I
                 USING (tenant_id = app_current_tenant())
                 WITH CHECK (tenant_id = app_current_tenant())', t);
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE submission_files;
DROP TABLE assignment_submissions;
DROP TABLE assignments;
