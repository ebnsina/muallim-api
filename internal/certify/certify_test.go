package certify

import (
	"strings"
	"testing"
)

/*
A serial is unguessable, and readable.

Unguessable because it is the whole of the credential: anybody holding one may
read the certificate it names. Readable because somebody types it off a printed
page, out of a frame, over the telephone.
*/
func TestNewSerial(t *testing.T) {
	t.Parallel()

	serial := NewSerial()

	if !strings.HasPrefix(serial, SerialPrefix+"-") {
		t.Errorf("%q does not say what it is", serial)
	}

	// The characters somebody misreads are not in it. I and 1, L and 1, O and 0 —
	// and U, which spells things nobody wants on a certificate.
	for _, forbidden := range "ILOU" {
		if strings.ContainsRune(strings.TrimPrefix(serial, SerialPrefix), forbidden) {
			t.Errorf("%q contains %q, which is read back as something else", serial, forbidden)
		}
	}

	// Grouped in fours, so it can be read aloud.
	groups := strings.Split(strings.TrimPrefix(serial, SerialPrefix+"-"), "-")
	if len(groups) != serialLength/4 {
		t.Errorf("%q has %d groups, want %d", serial, len(groups), serialLength/4)
	}
	for _, group := range groups {
		if len(group) != 4 {
			t.Errorf("%q has a group of %d characters", serial, len(group))
		}
	}
}

/*
Two serials are never the same.

Not a proof — a thousand draws cannot prove eighty bits — but a `math/rand` source
seeded the same way twice, or a counter somebody swapped in, fails this in the
first hundred.
*/
func TestSerialsDoNotRepeat(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool, 1000)
	for range 1000 {
		serial := NewSerial()
		if seen[serial] {
			t.Fatalf("%q was issued twice in a thousand draws", serial)
		}
		seen[serial] = true
	}
}

// Every character of the alphabet gets used, and nothing outside it does. A
// modulo that biased towards the low half would fail the first half of this.
func TestSerialsUseTheWholeAlphabet(t *testing.T) {
	t.Parallel()

	seen := map[rune]bool{}
	for range 500 {
		for _, r := range strings.ReplaceAll(strings.TrimPrefix(NewSerial(), SerialPrefix), "-", "") {
			if !strings.ContainsRune(alphabet, r) {
				t.Fatalf("%q is not in the alphabet", r)
			}
			seen[r] = true
		}
	}

	if len(seen) != len(alphabet) {
		t.Errorf("only %d of %d characters ever appeared", len(seen), len(alphabet))
	}
}

func TestRender(t *testing.T) {
	t.Parallel()

	fields := Fields{
		Learner: "Al-Khwarizmi",
		Course:  "Algebra from First Principles",
		Date:    "4 March 2026",
		Serial:  "CERT-ABCD",
	}

	tests := map[string]struct{ body, want string }{
		"every placeholder": {
			"{{learner}} finished {{course}} on {{date}}, {{serial}}.",
			"Al-Khwarizmi finished Algebra from First Principles on 4 March 2026, CERT-ABCD.",
		},
		"nothing to substitute":        {"Well done.", "Well done."},
		"name is an alias for learner": {"{{name}}", "Al-Khwarizmi"},
		"case does not matter":         {"{{LEARNER}}", "Al-Khwarizmi"},
		"whitespace inside is trimmed": {"{{ learner }}", "Al-Khwarizmi"},
		"repeated":                     {"{{learner}} and {{learner}}", "Al-Khwarizmi and Al-Khwarizmi"},

		/*
			A typo is printed, not silently swallowed.

			An empty string here reads "has completed the course  on 4 March" — a
			certificate somebody frames without looking closely. `{{corse}}` is a bug
			that reports itself, to the person who wrote the template.
		*/
		"an unknown placeholder is left alone": {"{{corse}}", "{{corse}}"},

		// A `{` that is not a placeholder, and a `{{` that never closes.
		"a lone brace":       {"{learner}", "{learner}"},
		"an unclosed brace":  {"finished {{course", "finished {{course"},
		"a brace at the end": {"done {", "done {"},
		"empty":              {"", ""},
		"just a placeholder": {"{{serial}}", "CERT-ABCD"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := Render(test.body, fields); got != test.want {
				t.Errorf("Render(%q) = %q, want %q", test.body, got, test.want)
			}
		})
	}
}

/*
Substitution happens once, over the template — not over its own output.

A learner who has named themselves `{{course}}` has not thereby printed the course
title where their name should be. The pass reads the template and writes the
result; it never re-reads what it wrote.
*/
func TestRenderDoesNotSubstituteItsOwnOutput(t *testing.T) {
	t.Parallel()

	fields := Fields{Learner: "{{course}}", Course: "Algebra"}

	if got := Render("{{learner}}", fields); got != "{{course}}" {
		t.Errorf("Render substituted its own output: %q", got)
	}
}

/*
The default template is valid, and renders to a clean description.

Its body names none of the four fields — they are shown in their own places by
whoever renders the certificate — so what this checks is that it is a legal
template and that nothing in it is left looking like an unfilled placeholder.
*/
func TestTheDefaultTemplateIsValidAndClean(t *testing.T) {
	t.Parallel()

	template := DefaultTemplate()
	if err := template.validate(); err != nil {
		t.Fatalf("the default template is invalid: %v", err)
	}
	if template.Title == "" {
		t.Error("the default template has no heading")
	}

	rendered := Render(template.Body, Fields{Learner: "L", Course: "C", Date: "D", Serial: "S"})
	if strings.Contains(rendered, "{{") {
		t.Errorf("the default template left something that looks like a placeholder: %q", rendered)
	}
}

func TestTemplateValidation(t *testing.T) {
	t.Parallel()

	good := Template{Name: "Formal", Title: "Certificate", Body: "Well done, {{learner}}."}
	if err := good.validate(); err != nil {
		t.Errorf("a well-formed template was refused: %v", err)
	}

	bad := map[string]Template{
		"no name":          {Title: "T", Body: "B"},
		"no title":         {Name: "N", Body: "B"},
		"no body":          {Name: "N", Title: "T"},
		"a name of spaces": {Name: "  ", Title: "T", Body: "B"},
	}

	for name, template := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := template.validate(); err == nil {
				t.Errorf("%s was accepted", name)
			}
		})
	}
}
