-- +goose Up

-- Admit the last four types and give them somewhere to keep what options and
-- accepted spellings cannot hold: a pin's image and hotspot regions, a graph's
-- expected points and tolerance, a drawing's backdrop. `spec` is nullable — every
-- older type stores nothing in it — and the answer-bearing halves (regions, points)
-- never leave the domain in a learner view.
--
-- puzzle grades exactly as ordering (its pieces are options with positions) and
-- draw_image is manual like open_ended; neither needs a spec of its own, though a
-- drawing may carry a backdrop image in one.
ALTER TABLE questions DROP CONSTRAINT questions_type_check;
ALTER TABLE questions ADD CONSTRAINT questions_type_check CHECK (type IN (
    'true_false', 'single_choice', 'multiple_choice', 'fill_blanks',
    'short_answer', 'ordering', 'matching', 'open_ended', 'range',
    'image_answering', 'image_matching',
    'puzzle', 'pin', 'graph', 'draw_image'));
ALTER TABLE questions ADD COLUMN spec jsonb;

ALTER TABLE bank_questions DROP CONSTRAINT bank_questions_type_check;
ALTER TABLE bank_questions ADD CONSTRAINT bank_questions_type_check CHECK (type IN (
    'true_false', 'single_choice', 'multiple_choice', 'fill_blanks',
    'short_answer', 'ordering', 'matching', 'open_ended', 'range',
    'image_answering', 'image_matching',
    'puzzle', 'pin', 'graph', 'draw_image'));
ALTER TABLE bank_questions ADD COLUMN spec jsonb;

-- +goose Down
ALTER TABLE questions DROP COLUMN spec;
ALTER TABLE questions DROP CONSTRAINT questions_type_check;
ALTER TABLE questions ADD CONSTRAINT questions_type_check CHECK (type IN (
    'true_false', 'single_choice', 'multiple_choice', 'fill_blanks',
    'short_answer', 'ordering', 'matching', 'open_ended', 'range',
    'image_answering', 'image_matching'));

ALTER TABLE bank_questions DROP COLUMN spec;
ALTER TABLE bank_questions DROP CONSTRAINT bank_questions_type_check;
ALTER TABLE bank_questions ADD CONSTRAINT bank_questions_type_check CHECK (type IN (
    'true_false', 'single_choice', 'multiple_choice', 'fill_blanks',
    'short_answer', 'ordering', 'matching', 'open_ended', 'range',
    'image_answering', 'image_matching'));
