// Package liveclass models bring-your-own-link live sessions: an instructor
// schedules a session on a course and stores a meeting URL, and enrolled learners
// see it and join. There is no video infrastructure — scheduling plus a link. It
// knows nothing about HTTP and references courses and users by id, never by import;
// who may read a session is an access decision the caller makes.
package liveclass

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinels. internal/httpapi maps each to a status; add a line to
// liveclass_errors_test.go in the same commit as a new one.
var (
	ErrNotFound       = errors.New("liveclass: not found")
	ErrInvalidSession = errors.New("liveclass: the session is not valid")
	ErrInvalidPage    = errors.New("liveclass: the page cursor is not valid")
)

// Audit actions.
const (
	ActionCreated = "live_session.created"
	ActionUpdated = "live_session.updated"
	ActionDeleted = "live_session.deleted"
)

// Session is one scheduled live class on a course.
type Session struct {
	ID          uuid.UUID
	CourseID    uuid.UUID
	Title       string
	Description string
	JoinURL     string
	StartsAt    time.Time
	EndsAt      *time.Time
	HostUserID  *uuid.UUID
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// NewSession is a session to create.
type NewSession struct {
	Title       string
	Description string
	JoinURL     string
	StartsAt    time.Time
	EndsAt      *time.Time
	HostUserID  *uuid.UUID
}

// SessionPatch changes a session. A nil field is left alone. ClearEndsAt and
// ClearJoinURL erase a value rather than leave it, so a caller can distinguish
// "unchanged" from "removed".
type SessionPatch struct {
	Title        *string
	Description  *string
	JoinURL      *string
	ClearJoinURL bool
	StartsAt     *time.Time
	EndsAt       *time.Time
	ClearEndsAt  bool
}

// validURL reports whether s is an http(s) URL — the only thing a browser can
// open as a meeting link.
func validURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Host != ""
}

func (n *NewSession) validate() error {
	n.Title = strings.TrimSpace(n.Title)
	if n.Title == "" {
		return fmt.Errorf("%w: give it a title", ErrInvalidSession)
	}
	if n.StartsAt.IsZero() {
		return fmt.Errorf("%w: it needs a start time", ErrInvalidSession)
	}
	if n.EndsAt != nil && n.EndsAt.Before(n.StartsAt) {
		return fmt.Errorf("%w: it cannot end before it starts", ErrInvalidSession)
	}
	if n.JoinURL != "" && !validURL(n.JoinURL) {
		return fmt.Errorf("%w: the join link must be an http(s) URL", ErrInvalidSession)
	}
	return nil
}

// apply folds the patch onto a session and validates the result, so a change is
// checked against the session it produces rather than the field in isolation.
func (p SessionPatch) apply(s *Session) error {
	if p.Title != nil {
		s.Title = strings.TrimSpace(*p.Title)
	}
	if p.Description != nil {
		s.Description = *p.Description
	}
	if p.ClearJoinURL {
		s.JoinURL = ""
	} else if p.JoinURL != nil {
		s.JoinURL = *p.JoinURL
	}
	if p.StartsAt != nil {
		s.StartsAt = *p.StartsAt
	}
	if p.ClearEndsAt {
		s.EndsAt = nil
	} else if p.EndsAt != nil {
		s.EndsAt = p.EndsAt
	}

	if s.Title == "" {
		return fmt.Errorf("%w: give it a title", ErrInvalidSession)
	}
	if s.EndsAt != nil && s.EndsAt.Before(s.StartsAt) {
		return fmt.Errorf("%w: it cannot end before it starts", ErrInvalidSession)
	}
	if s.JoinURL != "" && !validURL(s.JoinURL) {
		return fmt.Errorf("%w: the join link must be an http(s) URL", ErrInvalidSession)
	}
	return nil
}
