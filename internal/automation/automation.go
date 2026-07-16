/*
Package automation is a workspace's own rules for the mail it sends.

Every message this system sent before this package was one the code chose: a
verification, a reset, an invitation, a digest. A school that wanted to welcome
the people who enrol, or congratulate the ones who finish, had no way to say so
— which meant somebody had to remember, and nobody does.

A rule is a template and an event. The event says when; the subject and body say
what, with {{placeholders}} filled from what happened. The rendering is the whole
of the trust boundary here: an admin writes the template, and the values come from
the workspace's own records, so neither is a stranger — but a placeholder nobody
can fill is still a broken sentence in front of a learner, so an unknown one is
refused when the rule is written rather than discovered when it is sent.

The mail is enqueued in the transaction that recorded the event. A welcome email
for an enrolment that rolled back is a lie, and a queue that is asked to send one
has no way of knowing.
*/
package automation

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinels. internal/httpapi maps each to a status; add a line to errors_test.go
// in the same commit that adds one here.
var (
	// ErrNotFound: no such rule in this workspace.
	ErrNotFound = errors.New("automation: rule not found")

	// ErrInvalid: the rule as written cannot be saved.
	ErrInvalid = errors.New("automation: invalid rule")
)

// The events a rule may fire on. Closed, and matched by a CHECK on the table: a
// rule for an event nothing fires would never run, and its author would have no
// way to discover that.
const (
	// EventEnrolled fires when a learner joins a course: enrolling themselves,
	// being granted a seat, taking a bundle, or paying for it.
	//
	// A cohort import is the exception, and deliberately: it enrols hundreds of
	// people in one statement, and naming each of them to fill a template would be
	// a query per learner in the one path built to have none. The person who pasted
	// the list is the one telling that cohort they are on the course.
	EventEnrolled = "learner.enrolled"

	// EventCompleted fires when a learner finishes the last lesson of a course.
	EventCompleted = "course.completed"
)

// Events lists every event a rule may name, in the order a chooser should show
// them.
func Events() []string { return []string{EventEnrolled, EventCompleted} }

/*
Placeholders is what a rule may write for a given event, and what the sender
promises to fill.

Declared per event rather than globally: a rule cannot greet a course that has
nothing to do with the event it fires on, and telling an author which words work
is the difference between a template language and a guessing game.
*/
// The workspace's own name is not among them: resolving it where these events
// fire would mean a lookup the enrolment transaction has no other reason to do,
// and a placeholder that renders blank is worse than one that never existed.
var placeholders = map[string][]string{
	EventEnrolled:  {"learner_name", "course_title"},
	EventCompleted: {"learner_name", "course_title"},
}

// PlaceholdersFor lists the placeholders an event's templates may use. An unknown
// event has none, which is what makes it unwritable.
func PlaceholdersFor(event string) []string {
	return append([]string(nil), placeholders[event]...)
}

// ValidEvent says whether anything fires this event.
func ValidEvent(event string) bool { _, ok := placeholders[event]; return ok }

// Rule is one workspace's "when this happens, send that".
type Rule struct {
	ID        uuid.UUID
	Event     string
	Subject   string
	Body      string
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewRule describes a rule to create.
type NewRule struct {
	Event   string
	Subject string
	Body    string
	Enabled bool
}

// RulePatch updates a rule; a nil field is left alone. The event is not among
// them: a rule that fires on something else is a different rule, and silently
// repointing one an admin already trusts is not an edit.
type RulePatch struct {
	Subject *string
	Body    *string
	Enabled *bool
}

// Message is what an event knows about itself when it fires.
type Message struct {
	// To is the learner's address. A rule with nobody to send to sends nothing.
	To string

	// Vars fills the template's placeholders. A placeholder with no value here
	// renders empty rather than leaving its own braces in the sentence.
	Vars map[string]string
}

// placeholderPattern matches {{name}}, tolerating the spaces a person types.
var placeholderPattern = regexp.MustCompile(`{{\s*([a-z_]+)\s*}}`)

/*
render fills a template from vars.

A placeholder nobody supplies renders empty. That is deliberate: the alternative
is a learner reading "Welcome to {{course_title}}", and an empty gap is a
smaller failure than the machinery showing through. Rules are validated when they
are written, so this is the case where a value was legitimately blank, not where
the author guessed a name.
*/
func render(template string, vars map[string]string) string {
	return placeholderPattern.ReplaceAllStringFunc(template, func(match string) string {
		name := placeholderPattern.FindStringSubmatch(match)[1]
		return vars[name]
	})
}

// unknownPlaceholders lists the names a template uses that its event cannot fill.
func unknownPlaceholders(template, event string) []string {
	allowed := make(map[string]bool, len(placeholders[event]))
	for _, name := range placeholders[event] {
		allowed[name] = true
	}

	var unknown []string
	seen := map[string]bool{}
	for _, match := range placeholderPattern.FindAllStringSubmatch(template, -1) {
		name := match[1]
		if !allowed[name] && !seen[name] {
			seen[name] = true
			unknown = append(unknown, name)
		}
	}
	return unknown
}

func (n *NewRule) validate() error {
	n.Event = strings.TrimSpace(n.Event)
	if !ValidEvent(n.Event) {
		return fmt.Errorf("%w: nothing fires %q", ErrInvalid, n.Event)
	}

	n.Subject = strings.TrimSpace(n.Subject)
	if n.Subject == "" {
		return fmt.Errorf("%w: give it a subject", ErrInvalid)
	}
	n.Body = strings.TrimSpace(n.Body)
	if n.Body == "" {
		return fmt.Errorf("%w: give it something to say", ErrInvalid)
	}
	return checkPlaceholders(n.Event, n.Subject, n.Body)
}

func (p *RulePatch) validate(event string) error {
	if p.Subject != nil {
		s := strings.TrimSpace(*p.Subject)
		if s == "" {
			return fmt.Errorf("%w: the subject cannot be blank", ErrInvalid)
		}
		p.Subject = &s
	}
	if p.Body != nil {
		b := strings.TrimSpace(*p.Body)
		if b == "" {
			return fmt.Errorf("%w: the message cannot be blank", ErrInvalid)
		}
		p.Body = &b
	}

	var subject, body string
	if p.Subject != nil {
		subject = *p.Subject
	}
	if p.Body != nil {
		body = *p.Body
	}
	return checkPlaceholders(event, subject, body)
}

// checkPlaceholders refuses a template that names something its event cannot
// fill — caught while the author is looking at it, rather than by a learner.
func checkPlaceholders(event string, templates ...string) error {
	for _, t := range templates {
		if unknown := unknownPlaceholders(t, event); len(unknown) > 0 {
			return fmt.Errorf("%w: nothing fills %s here; this event offers %s",
				ErrInvalid,
				strings.Join(quoted(unknown), ", "),
				strings.Join(quoted(placeholders[event]), ", "))
		}
	}
	return nil
}

func quoted(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = "{{" + n + "}}"
	}
	return out
}
