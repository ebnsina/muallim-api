package main

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

/*
The same row is the same id, every run.

This is what `-seed` promises and what `uuid.New()` quietly broke: reseeding used
to give every course, lesson, and option a new face, so a page anyone had open
became a 404 and any script holding an id had to be rewritten.
*/
func TestIdIsStable(t *testing.T) {
	t.Parallel()

	first := id("course", "acme", "algebra-0")
	if second := id("course", "acme", "algebra-0"); first != second {
		t.Errorf("the same row came out as %s and then %s", first, second)
	}

	// And not the nil uuid, which is what a mistake in the derivation would give
	// every row at once.
	if first == uuid.Nil {
		t.Error("a derived id is the nil uuid")
	}
}

/*
Different rows are different ids.

The separator is what makes the key injective. Without it `("ab", "c")` and
`("a", "bc")` hash the same bytes, and two rows become one — a foreign key
pointing at a sibling, silently, in data nobody reads closely because it is only
the seed.
*/
func TestIdSeparatesItsParts(t *testing.T) {
	t.Parallel()

	if id("lesson", "ab", "c") == id("lesson", "a", "bc") {
		t.Error("two different rows derived the same id: the parts are not separated")
	}
}

/*
A kind is a namespace.

A topic at position 2 of a course and a lesson at position 2 of a topic are
addressed by the same numbers. Only the prefix keeps them apart, and a collision
here would be a lesson whose id is a topic's — a foreign key that resolves, to
the wrong table's row.
*/
func TestIdNamespacesByKind(t *testing.T) {
	t.Parallel()

	parent := uuid.New()
	if id("topic", parent, 2) == id("lesson", parent, 2) {
		t.Error("a topic and a lesson at the same position share an id")
	}

	// The one that matters most: a wrong answer names an option that does not
	// exist. If it could name a real one, a seeded wrong answer would be right.
	attempt, question := uuid.New(), uuid.New()
	for o := range 4 {
		if id("wrong", attempt, question) == id("option", question, o) {
			t.Fatal("a wrong answer names a real option")
		}
	}
}

// Every id in the seeder is derived, so the whole database is a function of the
// flags. Nothing here may reach for the clock or the entropy pool.
func TestEveryPartTypeContributes(t *testing.T) {
	t.Parallel()

	seen := map[uuid.UUID]string{}

	for _, key := range []struct {
		name  string
		value uuid.UUID
	}{
		{"no parts", id("user")},
		{"a string", id("user", "ada@example.test")},
		{"an int", id("user", 1)},
		{"the string of that int", id("user", "1")},
		{"a uuid", id("user", uuid.Nil)},
		{"two parts", id("user", 1, 2)},
	} {
		// `id("user", 1)` and `id("user", "1")` are deliberately the same: %v renders
		// both as "1", and no caller mixes the two for one column. Everything else is
		// distinct.
		if key.name == "the string of that int" {
			if key.value != id("user", 1) {
				t.Error("an int and its string rendered differently; that is fine, but say so here")
			}
			continue
		}

		if previous, clash := seen[key.value]; clash {
			t.Errorf("%s collides with %s", key.name, previous)
		}
		seen[key.value] = key.name
	}
}

/*
The database, and nothing else the URL carries.

A connection string holds a password. Printing one puts a secret in a terminal,
in a scrollback, and in whatever CI keeps of its logs — and this line is printed
on every seed, right above four sets of credentials, where nobody would look
twice at a fifth.
*/
func TestDatabaseNameLeaksNothing(t *testing.T) {
	t.Parallel()

	const secret = "hunter2"
	url := "postgres://lms:" + secret + "@localhost:5432/lms_test?sslmode=disable"

	got := databaseName(url)
	if got != "lms_test" {
		t.Errorf("databaseName = %q, want %q", got, "lms_test")
	}
	if strings.Contains(got, secret) {
		t.Error("the password is in the line the seeder prints")
	}

	// A URL nobody can parse is a question mark, not a panic and not the raw string.
	if got := databaseName("://not a url"); strings.Contains(got, "not a url") {
		t.Errorf("an unparseable URL was printed verbatim: %q", got)
	}
}
