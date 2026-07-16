// Package coursebuild is a standalone visual course-structure designer. A blueprint
// is a curriculum sketch — modules holding lessons — kept as one jsonb document so a
// drag-and-drop editor saves it in a single write. It is deliberately independent of
// the `catalog` domain: it references no course, topic or lesson, and nothing in the
// catalogue references it. It knows nothing about HTTP.
package coursebuild

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinels. internal/httpapi maps each to a status; add a line to
// coursebuild_errors_test.go in the same commit as a new one.
var (
	ErrNotFound         = errors.New("coursebuild: not found")
	ErrInvalidBlueprint = errors.New("coursebuild: the blueprint is not valid")
	ErrInvalidPage      = errors.New("coursebuild: the page cursor is not valid")
)

// Lesson kinds a blueprint node may carry.
const (
	KindVideo      = "video"
	KindText       = "text"
	KindQuiz       = "quiz"
	KindAssignment = "assignment"
	KindFile       = "file"
)

var lessonKinds = map[string]bool{
	KindVideo: true, KindText: true, KindQuiz: true, KindAssignment: true, KindFile: true,
}

// Bounds.
const (
	MaxNameLen  = 300
	MaxModules  = 500
	MaxLessons  = 500
	MaxTitleLen = 300
	MaxNotesLen = 5000
	MaxDescLen  = 5000
)

// Audit actions.
const (
	ActionCreated = "course_blueprint.created"
	ActionUpdated = "course_blueprint.updated"
	ActionDeleted = "course_blueprint.deleted"
)

// Blueprint is one course-structure sketch. Structure is the raw jsonb document —
// an array of modules — passed through untouched once it has been validated.
type Blueprint struct {
	ID          uuid.UUID
	Name        string
	Description string
	Structure   json.RawMessage
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// NewBlueprint is a blueprint to create.
type NewBlueprint struct {
	Name        string
	Description string
	Structure   json.RawMessage
}

// BlueprintPatch is a partial update: a nil field is left unchanged. Structure is
// nil to leave the document as it stands, or a new document to replace it wholesale.
type BlueprintPatch struct {
	Name        *string
	Description *string
	Structure   json.RawMessage
}

// module and lesson mirror the on-the-wire jsonb shape, used only for validation.
type module struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Lessons []lesson `json:"lessons"`
}

type lesson struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Kind  string `json:"kind"`
	Notes string `json:"notes"`
}

// validateStructure confirms the document is an array of modules, each with a title
// and a lessons array whose kinds are known. An empty document is the array `[]`.
func validateStructure(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var mods []module
	if err := dec.Decode(&mods); err != nil {
		return fmt.Errorf("%w: structure must be an array of modules", ErrInvalidBlueprint)
	}
	if len(mods) > MaxModules {
		return fmt.Errorf("%w: too many modules", ErrInvalidBlueprint)
	}
	for i, m := range mods {
		if strings.TrimSpace(m.Title) == "" {
			return fmt.Errorf("%w: module %d needs a title", ErrInvalidBlueprint, i+1)
		}
		if len(m.Title) > MaxTitleLen {
			return fmt.Errorf("%w: module %d title is too long", ErrInvalidBlueprint, i+1)
		}
		if len(m.Lessons) > MaxLessons {
			return fmt.Errorf("%w: module %d has too many lessons", ErrInvalidBlueprint, i+1)
		}
		for j, l := range m.Lessons {
			if strings.TrimSpace(l.Title) == "" {
				return fmt.Errorf("%w: module %d lesson %d needs a title", ErrInvalidBlueprint, i+1, j+1)
			}
			if len(l.Title) > MaxTitleLen {
				return fmt.Errorf("%w: module %d lesson %d title is too long", ErrInvalidBlueprint, i+1, j+1)
			}
			if len(l.Notes) > MaxNotesLen {
				return fmt.Errorf("%w: module %d lesson %d notes are too long", ErrInvalidBlueprint, i+1, j+1)
			}
			if !lessonKinds[l.Kind] {
				return fmt.Errorf("%w: module %d lesson %d has an unknown kind %q", ErrInvalidBlueprint, i+1, j+1, l.Kind)
			}
		}
	}
	return nil
}

// normalise defaults an absent structure to the empty array so the column always
// holds valid jsonb.
func normalise(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("[]")
	}
	return raw
}

func (n *NewBlueprint) validate() error {
	n.Name = strings.TrimSpace(n.Name)
	if n.Name == "" {
		return fmt.Errorf("%w: give it a name", ErrInvalidBlueprint)
	}
	if len(n.Name) > MaxNameLen {
		return fmt.Errorf("%w: the name is too long", ErrInvalidBlueprint)
	}
	if len(n.Description) > MaxDescLen {
		return fmt.Errorf("%w: the description is too long", ErrInvalidBlueprint)
	}
	if err := validateStructure(n.Structure); err != nil {
		return err
	}
	n.Structure = normalise(n.Structure)
	return nil
}

func (p *BlueprintPatch) validate() error {
	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" {
			return fmt.Errorf("%w: the name cannot be blank", ErrInvalidBlueprint)
		}
		if len(name) > MaxNameLen {
			return fmt.Errorf("%w: the name is too long", ErrInvalidBlueprint)
		}
		p.Name = &name
	}
	if p.Description != nil && len(*p.Description) > MaxDescLen {
		return fmt.Errorf("%w: the description is too long", ErrInvalidBlueprint)
	}
	if p.Structure != nil {
		if err := validateStructure(p.Structure); err != nil {
			return err
		}
	}
	return nil
}
