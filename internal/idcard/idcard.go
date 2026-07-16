// Package idcard is the ID-card designer: a standalone visual-design tool for
// laying out a student or staff ID card on a canvas. It is deliberately
// independent of the staff and academics domains — it stores reusable templates
// (subject, orientation, palette, background, and positioned fields), never a
// card issued to a real person. It knows nothing about HTTP and returns sentinel
// errors the http layer maps to statuses.
package idcard

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinels. internal/httpapi maps each to a status; add a line to
// idcard_errors_test.go in the same commit as a new one.
var (
	ErrNotFound        = errors.New("idcard: not found")
	ErrInvalidTemplate = errors.New("idcard: the template is not valid")
	ErrInvalidLayout   = errors.New("idcard: the layout is not valid")
	ErrInvalidPage     = errors.New("idcard: the page cursor is not valid")
)

// Subject values.
const (
	SubjectStudent = "student"
	SubjectStaff   = "staff"
)

// Orientation values.
const (
	OrientationPortrait  = "portrait"
	OrientationLandscape = "landscape"
)

// Bounds.
const MaxLayoutElements = 200

// Audit actions.
const (
	ActionTemplateCreated       = "id_card_template.created"
	ActionTemplateUpdated       = "id_card_template.updated"
	ActionTemplateDeleted       = "id_card_template.deleted"
	ActionTemplateBackgroundSet = "id_card_template.background_set"
)

// elementKinds is the closed set of field kinds a layout may contain.
var elementKinds = map[string]bool{
	"name":          true,
	"photo":         true,
	"id_number":     true,
	"class_or_role": true,
	"valid_until":   true,
	"blood_group":   true,
	"school_name":   true,
	"text":          true,
}

// Template is one saved ID-card layout: a subject, a canvas orientation, a
// palette, an optional background image, and the positioned fields. Layout is
// carried as raw JSON — the tool owns its shape, and the domain only validates it.
type Template struct {
	ID              uuid.UUID
	Name            string
	Subject         string
	Orientation     string
	Accent          string
	BackgroundColor string
	BackgroundKey   string
	Layout          json.RawMessage
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// NewTemplate is a template to create.
type NewTemplate struct {
	Name            string
	Subject         string
	Orientation     string
	Accent          string
	BackgroundColor string
	Layout          json.RawMessage
}

// TemplatePatch updates a template's editable fields in place. A nil field is
// left untouched; background is set through its own confirm step, not here.
type TemplatePatch struct {
	Name            *string
	Subject         *string
	Orientation     *string
	Accent          *string
	BackgroundColor *string
	Layout          json.RawMessage
}

// element is one positioned field in a layout, validated field by field.
type element struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	W          float64 `json:"w"`
	FontSize   float64 `json:"fontSize"`
	FontWeight int     `json:"fontWeight"`
	Color      string  `json:"color"`
	Align      string  `json:"align"`
	Text       string  `json:"text"`
}

func validSubject(s string) bool {
	return s == SubjectStudent || s == SubjectStaff
}

func validOrientation(o string) bool {
	return o == OrientationPortrait || o == OrientationLandscape
}

func inUnit(v float64) bool { return v >= 0 && v <= 1 }

// validateLayout ensures the raw JSON is an array of elements whose kinds are
// known and whose x/y/w are fractions in [0,1]. An empty or absent layout is the
// empty array. It returns ErrInvalidLayout on any violation.
func validateLayout(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage("[]"), nil
	}
	var elems []element
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, fmt.Errorf("%w: it must be an array of elements", ErrInvalidLayout)
	}
	if len(elems) > MaxLayoutElements {
		return nil, fmt.Errorf("%w: too many elements", ErrInvalidLayout)
	}
	for i, e := range elems {
		if !elementKinds[e.Kind] {
			return nil, fmt.Errorf("%w: element %d has an unknown kind %q", ErrInvalidLayout, i, e.Kind)
		}
		if !inUnit(e.X) || !inUnit(e.Y) || !inUnit(e.W) {
			return nil, fmt.Errorf("%w: element %d has x/y/w outside [0,1]", ErrInvalidLayout, i)
		}
	}
	// Re-marshal so what is stored is exactly the validated shape.
	clean, err := json.Marshal(elems)
	if err != nil {
		return nil, fmt.Errorf("%w: it could not be encoded", ErrInvalidLayout)
	}
	return clean, nil
}

func (n *NewTemplate) validate() error {
	n.Name = strings.TrimSpace(n.Name)
	if n.Subject == "" {
		n.Subject = SubjectStudent
	}
	if !validSubject(n.Subject) {
		return fmt.Errorf("%w: subject must be student or staff", ErrInvalidTemplate)
	}
	if n.Orientation == "" {
		n.Orientation = OrientationPortrait
	}
	if !validOrientation(n.Orientation) {
		return fmt.Errorf("%w: orientation must be portrait or landscape", ErrInvalidTemplate)
	}
	n.Accent = strings.TrimSpace(n.Accent)
	n.BackgroundColor = strings.TrimSpace(n.BackgroundColor)
	clean, err := validateLayout(n.Layout)
	if err != nil {
		return err
	}
	n.Layout = clean
	return nil
}

func (p *TemplatePatch) validate() error {
	if p.Name != nil {
		v := strings.TrimSpace(*p.Name)
		p.Name = &v
	}
	if p.Subject != nil {
		if !validSubject(*p.Subject) {
			return fmt.Errorf("%w: subject must be student or staff", ErrInvalidTemplate)
		}
	}
	if p.Orientation != nil {
		if !validOrientation(*p.Orientation) {
			return fmt.Errorf("%w: orientation must be portrait or landscape", ErrInvalidTemplate)
		}
	}
	if p.Accent != nil {
		v := strings.TrimSpace(*p.Accent)
		p.Accent = &v
	}
	if p.BackgroundColor != nil {
		v := strings.TrimSpace(*p.BackgroundColor)
		p.BackgroundColor = &v
	}
	if p.Layout != nil {
		clean, err := validateLayout(p.Layout)
		if err != nil {
			return err
		}
		p.Layout = clean
	}
	return nil
}
