-- +goose Up

-- Admit the image types. Each grades exactly as its text cousin — image answering
-- as single choice, image matching as matching — and the option's image URL rides
-- in the existing `content`/`match_content` columns, so the only schema change is
-- widening the two type checks.
ALTER TABLE questions DROP CONSTRAINT questions_type_check;
ALTER TABLE questions ADD CONSTRAINT questions_type_check CHECK (type IN (
    'true_false', 'single_choice', 'multiple_choice', 'fill_blanks',
    'short_answer', 'ordering', 'matching', 'open_ended', 'range',
    'image_answering', 'image_matching'));

ALTER TABLE bank_questions DROP CONSTRAINT bank_questions_type_check;
ALTER TABLE bank_questions ADD CONSTRAINT bank_questions_type_check CHECK (type IN (
    'true_false', 'single_choice', 'multiple_choice', 'fill_blanks',
    'short_answer', 'ordering', 'matching', 'open_ended', 'range',
    'image_answering', 'image_matching'));

-- +goose Down
ALTER TABLE questions DROP CONSTRAINT questions_type_check;
ALTER TABLE questions ADD CONSTRAINT questions_type_check CHECK (type IN (
    'true_false', 'single_choice', 'multiple_choice', 'fill_blanks',
    'short_answer', 'ordering', 'matching', 'open_ended', 'range'));

ALTER TABLE bank_questions DROP CONSTRAINT bank_questions_type_check;
ALTER TABLE bank_questions ADD CONSTRAINT bank_questions_type_check CHECK (type IN (
    'true_false', 'single_choice', 'multiple_choice', 'fill_blanks',
    'short_answer', 'ordering', 'matching', 'open_ended', 'range'));
