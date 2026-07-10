package assign

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

/*
The key is a uuid under a prefix, and nothing a learner typed appears in it.

This is the check that makes filename sanitising a presentation problem rather
than a security one. If a name ever reached the key, every one of the cases in
TestCleanFilename would become a path traversal instead of an ugly label.
*/
func TestTheObjectKeyHoldsNothingAUserChose(t *testing.T) {
	t.Parallel()

	tenant, assignment, user := uuid.New(), uuid.New(), uuid.New()

	key := objectKey(tenant, assignment, user)
	prefix := keyPrefix(tenant, assignment, user)

	if !strings.HasPrefix(key, prefix+"/") {
		t.Fatalf("key %q is not under prefix %q", key, prefix)
	}

	// The last segment is a uuid and nothing else.
	last := key[len(prefix)+1:]
	if _, err := uuid.Parse(last); err != nil {
		t.Errorf("the key's last segment %q is not a uuid", last)
	}

	// Two calls never collide, so two files of the same name never overwrite.
	if objectKey(tenant, assignment, user) == key {
		t.Error("two keys for the same learner came out identical")
	}

	// And a different learner is a different prefix. This is what makes the
	// prefix check in AttachFile an authorisation check.
	if strings.HasPrefix(objectKey(tenant, assignment, uuid.New()), prefix+"/") {
		t.Error("another learner's key lives under this learner's prefix")
	}
	if strings.HasPrefix(objectKey(uuid.New(), assignment, user), prefix+"/") {
		t.Error("another workspace's key lives under this workspace's prefix")
	}
}

/*
Names arrive from a browser, which got them from a filesystem, which got them
from a person.

The name is never a path — the key is a uuid — so what remains is a name that
has to survive being written into a `Content-Disposition` header and read back
out by a browser. A quote or a newline in there ends the header early and starts
one the uploader chose.
*/
func TestCleanFilename(t *testing.T) {
	t.Parallel()

	tests := map[string]struct{ in, want string }{
		"an ordinary name":     {"essay.pdf", "essay.pdf"},
		"unicode survives":     {"مقالة.pdf", "مقالة.pdf"},
		"spaces survive":       {"my final essay.pdf", "my final essay.pdf"},
		"a unix directory":     {"/etc/passwd", "passwd"},
		"a windows directory":  {`C:\Users\ada\essay.pdf`, "essay.pdf"},
		"a traversal":          {"../../../../etc/shadow", "shadow"},
		"a trailing separator": {"folder/", "file"},
		"a bare dot":           {".", "file"},
		"two dots":             {"..", "file"},
		"nothing at all":       {"", "file"},
		"only whitespace":      {"   \t  ", "file"},

		// The header injections. A newline in a filename is a second header.
		"a newline":        {"essay\r\nX-Evil: yes.pdf", "essayX-Evil: yes.pdf"},
		"a NUL":            {"essay\x00.pdf", "essay.pdf"},
		"a vertical tab":   {"essay\v.pdf", "essay.pdf"},
		"collapsed spaces": {"essay     final.pdf", "essay final.pdf"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := cleanFilename(test.in)
			if got != test.want {
				t.Errorf("cleanFilename(%q) = %q, want %q", test.in, got, test.want)
			}

			// Whatever comes out, these are true of all of it.
			if strings.ContainsAny(got, "\x00\r\n") {
				t.Errorf("%q still holds a character that ends a header", got)
			}
			if strings.ContainsAny(got, `/\`) {
				t.Errorf("%q still holds a path separator", got)
			}
			if got == "" {
				t.Error("a file with no name is a file nobody can click")
			}
		})
	}
}

// The limit is the filesystem's, and the filesystem counts bytes. Trimming at a
// byte boundary can cut a rune in half, and half a character is worse than a
// shorter name.
func TestALongNameIsCutOnARuneBoundary(t *testing.T) {
	t.Parallel()

	// Each of these is three bytes. 100 of them is 300, comfortably over the cap.
	long := strings.Repeat("中", 100)

	got := cleanFilename(long)
	if len(got) > MaxFilenameLength {
		t.Errorf("cleaned name is %d bytes, over the %d cap", len(got), MaxFilenameLength)
	}
	if !utf8.ValidString(got) {
		t.Errorf("cleaned name is not valid UTF-8: %q", got)
	}
	if got == "" {
		t.Error("the whole name was cut away")
	}
}

func TestNewAssignmentValidation(t *testing.T) {
	t.Parallel()

	good := NewAssignment{Title: "Essay", Points: 100, PassingPoints: 50, MaxFiles: 3, MaxBytes: 1 << 20}
	if err := good.validate(); err != nil {
		t.Errorf("a well-formed assignment was refused: %v", err)
	}

	bad := map[string]NewAssignment{
		"no title":        {Title: "  ", Points: 10, MaxFiles: 1, MaxBytes: 1},
		"negative points": {Title: "E", Points: -1, MaxFiles: 1, MaxBytes: 1},
		"a pass mark above the points": {
			Title: "E", Points: 10, PassingPoints: 11, MaxFiles: 1, MaxBytes: 1,
		},
		"a negative pass mark": {Title: "E", Points: 10, PassingPoints: -1, MaxFiles: 1, MaxBytes: 1},
		"no files at all":      {Title: "E", Points: 10, MaxFiles: 0, MaxBytes: 1},
		"too many files":       {Title: "E", Points: 10, MaxFiles: MaxFilesEver + 1, MaxBytes: 1},
		"a file of no bytes":   {Title: "E", Points: 10, MaxFiles: 1, MaxBytes: 0},
		"a file of a gigabyte and one": {
			Title: "E", Points: 10, MaxFiles: 1, MaxBytes: MaxBytesEver + 1,
		},
	}

	for name, assignment := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := assignment.validate(); err == nil {
				t.Errorf("%s was accepted", name)
			}
		})
	}
}

/*
A patch is checked against the assignment it produces, not the one it replaces.

Raising the pass mark to 90 and then lowering the points to 50 is two legal
requests that together make an assignment nobody can pass. `merge` is what the
second request is validated against.
*/
func TestAPatchIsValidatedAgainstItsResult(t *testing.T) {
	t.Parallel()

	existing := Assignment{Title: "Essay", Points: 100, PassingPoints: 90, MaxFiles: 3, MaxBytes: 1 << 20}

	fifty := 50
	if err := merge(existing, AssignmentPatch{Points: &fifty}).validate(); err == nil {
		t.Error("lowering the points below the pass mark was accepted")
	}

	// And lowering both together is fine.
	forty := 40
	if err := merge(existing, AssignmentPatch{Points: &fifty, PassingPoints: &forty}).validate(); err != nil {
		t.Errorf("lowering both was refused: %v", err)
	}
}

/*
A patch carries three answers about the deadline, not two.

Absent leaves it, a value sets it, and cleared removes it. A `*time.Time` alone
cannot say the difference between the first and the last, and an author who
could set a deadline but never take one off would have to delete the assignment
— and every submission under it — to fix a typo.
*/
func TestPatchingTheDeadline(t *testing.T) {
	t.Parallel()

	due := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	later := due.Add(24 * time.Hour)

	existing := Assignment{
		Title: "Essay", Points: 100, PassingPoints: 50,
		MaxFiles: 3, MaxBytes: 1 << 20, DueAt: &due, AllowLate: true,
	}

	if got := merge(existing, AssignmentPatch{}).DueAt; got == nil || !got.Equal(due) {
		t.Errorf("an empty patch moved the deadline to %v", got)
	}

	if got := merge(existing, AssignmentPatch{DueAt: &later}).DueAt; got == nil || !got.Equal(later) {
		t.Errorf("setting the deadline gave %v, want %v", got, later)
	}

	if got := merge(existing, AssignmentPatch{ClearDueAt: true}).DueAt; got != nil {
		t.Errorf("clearing the deadline left %v", got)
	}

	// A deadline that is being removed and set in the same breath is removed.
	if got := merge(existing, AssignmentPatch{DueAt: &later, ClearDueAt: true}).DueAt; got != nil {
		t.Errorf("clearing lost to setting, leaving %v", got)
	}
}

// The other two fields a patch used to drop on the floor.
func TestPatchingLateness(t *testing.T) {
	t.Parallel()

	existing := Assignment{Title: "Essay", Points: 100, MaxFiles: 3, MaxBytes: 1 << 20, AllowLate: true}

	if merge(existing, AssignmentPatch{}).AllowLate != true {
		t.Error("an empty patch changed the late policy")
	}

	no := false
	if merge(existing, AssignmentPatch{AllowLate: &no}).AllowLate != false {
		t.Error("turning late work off did nothing")
	}
}
