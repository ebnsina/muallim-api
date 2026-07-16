# Academics

The institution spine: the calendar and the structure that any school, college,
madrasa, or coaching centre organises itself around, and the people it is for.
Exams, fees, notices, hifz, admissions, library, transport and hostel all reference
this layer **by id, never by import** — it is the one thing they agree on, and the
reason none of them has to know about the others. A modular-monolith domain like
the rest: it knows nothing about HTTP, and every table is tenant-scoped with RLS
behind the application's own `tenant_id` filter.

**The institution type sets vocabulary and defaults, never schema.** A workspace is
a `school`, `college`, `madrasa`, or `coaching` centre — a `CHECK`-constrained
column on `tenants`, matched by `ValidInstitutionType` in Go. It chooses a madrasa's
grading ladder or a coaching centre's batches; it does not choose a table. A type
that forked the schema would mean four products maintained as one, and a school that
also runs a madrasa wing could belong to neither.

## Model

### The calendar

- **`academic_years`** — the calendar everything is scheduled within. `name`,
  `starts_on`, `ends_on`, `is_current`. Names are unique per workspace,
  case-insensitively; `CHECK (ends_on > starts_on)`.
- **`terms`** — a semester or quarter within a year. `name`, `starts_on`,
  `ends_on`, and a `position` in the author's order, cascading from the year.

**At most one year is current, and the database is what says so** — a partial
unique index, `academic_years_one_current ON academic_years (tenant_id) WHERE
is_current`, rather than a rule the caller is trusted to keep. Every read that asks
"which year is this?" would otherwise have to pick between two, and there is no
right answer to that. The swap runs as two statements inside one transaction —
clear the old, set the new — because the partial index forbids two current years
even for the instant a single multi-row `UPDATE` would take to cross over.

### The structure

- **`grade_levels`** — a class: Class 6, Dakhil 1st Year, Batch A. `rank` orders
  them by seniority, so a listing reads junior to senior instead of alphabetising
  "Class 10" above "Class 2".
- **`sections`** — a division within a class (6-A, 6-B). `capacity` is a soft cap;
  zero means unset. Unique per class, case-insensitively.
- **`subjects`** — what the institution teaches (Bangla, Mathematics, Quran, Fiqh).
  Workspace-level; exams mark against them. A `code` is optional but unique when
  present — `WHERE code <> ''`, because a blank code is not a collision.

### The people

- **`students`** — `admission_no` (the school's own key for a student, unique per
  workspace case-insensitively), `full_name`, an optional `grade_level_id` and
  `section_id`, `roll`, a `status` of `active` / `inactive` / `graduated` /
  `transferred`, and an optional `user_id`.
- **`guardians`** — a contact: `full_name`, `phone`, `email`, `relation`, and an
  optional `user_id` (see [portal.md](portal.md)).
- **`student_guardians`** — the link, with `is_primary`. A student has one primary
  contact, whom notices reach first; naming a new one demotes the old in the same
  transaction. Unique per `(student, guardian)` pair.

`user_id` is nullable on both, and never assumed. A young child on a roll has no
login and an older learner might; a guardian is a contact before they are ever an
account. Both are `ON DELETE SET NULL` — removing the login leaves the record.

The roster is the only listing here that is **keyset-paginated**, by `(full_name,
id)`, because a school has thousands of students. Years, classes and subjects are
bounded instead (`MaxYears` 100, `MaxClasses` 200, `MaxSubjects` 300): the cap is
generous enough that reaching it is a mistake rather than a school. The roster's two
query shapes each get their own statement and their own covering index —
`students_tenant_name_idx` and `students_grade_name_idx` — rather than one query
with `grade_level_id = $x OR $x IS NULL`, which is not sargable, so the planner
would abandon the grade index even when a class *is* named and `EXPLAIN` would show
the Sort node to prove it.

## Promotion

`POST /v1/classes/{id}/promote` moves every **active** student in a class into a
target class (optionally placing them in a section). With no target they are
`graduated` — the final class has nowhere higher to go. One `UPDATE`, so the whole
class moves together or not at all, and `status = 'active'` in the predicate makes
a re-run safe. Promoting a class into itself is refused (`ErrInvalidPromotion`).

## Endpoints

All under `academics:manage`, admin-only, bearer auth. Setting up the institution is
an administrative act, not a teaching one: an instructor authors courses but does
not redraw the school. Attendance (`/v1/attendance`, `/v1/students/{id}/attendance`)
and the timetable (`/v1/sections/{id}/timetable`) are served by this same package
under the same permission, and hang off the sections and students below.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` `PUT` | `/v1/academics/institution-type` | What kind of institution this workspace is. |
| `GET` `POST` | `/v1/academic-years` | Years; create one. |
| `PUT` | `/v1/academic-years/{id}/current` | Mark a year current, clearing the old one. |
| `DELETE` | `/v1/academic-years/{id}` | Remove a year, and its terms by cascade. |
| `GET` `POST` | `/v1/academic-years/{id}/terms` | A year's terms, in position order; append one. |
| `DELETE` | `/v1/terms/{id}` | Remove a term. |
| `GET` `POST` | `/v1/classes` | Classes by rank; create one. |
| `DELETE` | `/v1/classes/{id}` | Remove a class. |
| `POST` | `/v1/classes/{id}/promote` | Promote a class's students, or graduate them. Body: `to_grade_level_id?`, `to_section_id?`. Returns `moved` and `graduated`. |
| `GET` `POST` | `/v1/classes/{id}/sections` | A class's sections; add one. |
| `DELETE` | `/v1/sections/{id}` | Remove a section. |
| `GET` `POST` | `/v1/subjects` | The subject catalogue; add one. |
| `DELETE` | `/v1/subjects/{id}` | Remove a subject. |
| `GET` | `/v1/students` | The roster, by name. Filter by `grade_level_id`. Keyset `cursor` + `has_more`. |
| `POST` | `/v1/students` | Admit a student. Body: `admission_no`, `full_name`, `grade_level_id?`, `section_id?`, `roll?`. 201. |
| `GET` `PATCH` `DELETE` | `/v1/students/{id}` | One student; amend or remove them. |
| `GET` `POST` | `/v1/students/{id}/guardians` | A student's guardians, primary first; add one. |
| `DELETE` | `/v1/students/{id}/guardians/{guardian_id}` | Unlink a guardian from a student. |

## Deliberately not here

Marks, invoices, and the memorisation log. `exams`, `fees` and `hifz` reference a
student or a term by id and own their own tables; folding them in would make this
package the thing every other domain imports, which is the dependency rule inverted.
Admitting a student from an application is likewise not here — see
[admissions.md](admissions.md): the intake queue records a decision, and the
coordinator in `internal/httpapi` is the only place that knows both packages exist.

## Errors

| Sentinel | Status | When |
| --- | --- | --- |
| `ErrNotFound` | 404 | No such record in this workspace. |
| `ErrNameTaken` | 409 | A year, class, section or subject already uses that name here. |
| `ErrAdmissionTaken` | 409 | That admission number is already used in this workspace. |
| `ErrInvalidPage` | 422 | The roster cursor did not decode. |
| `ErrInvalidYear` / `ErrInvalidTerm` / `ErrInvalidClass` / `ErrInvalidSection` / `ErrInvalidSubject` / `ErrInvalidStudent` / `ErrInvalidGuardian` / `ErrInvalidAttendance` / `ErrInvalidPeriod` / `ErrInvalidPromotion` / `ErrInvalidInstitutionType` | 422 | The submitted details do not make sense — a blank name, a year that ends before it starts, a class promoted into itself, an unknown institution type. |
