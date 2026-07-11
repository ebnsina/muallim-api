-- +goose Up

-- Admit the range type. Its bounds ride in the existing `accepted` column, so the
-- only schema change is widening the type check.
ALTER TABLE questions DROP CONSTRAINT questions_type_check;
ALTER TABLE questions ADD CONSTRAINT questions_type_check CHECK (type IN (
    'true_false', 'single_choice', 'multiple_choice', 'fill_blanks',
    'short_answer', 'ordering', 'matching', 'open_ended', 'range'));

-- +goose Down
ALTER TABLE questions DROP CONSTRAINT questions_type_check;
ALTER TABLE questions ADD CONSTRAINT questions_type_check CHECK (type IN (
    'true_false', 'single_choice', 'multiple_choice', 'fill_blanks',
    'short_answer', 'ordering', 'matching', 'open_ended'));
