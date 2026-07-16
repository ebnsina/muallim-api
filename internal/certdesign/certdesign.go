// Package certdesign is the Certificate Designer: a standalone visual-design tool
// for laying out a certificate on a canvas. It is deliberately independent of the
// certify domain — it stores reusable designs (orientation, palette, background,
// and positioned elements), never a certificate issued to a learner. It knows
// nothing about HTTP and returns sentinel errors the http layer maps to statuses.
package certdesign

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinels. internal/httpapi maps each to a status; add a line to
// certdesign_errors_test.go in the same commit as a new one.
var (
	ErrNotFound      = errors.New("certdesign: not found")
	ErrInvalidDesign = errors.New("certdesign: the design is not valid")
	ErrInvalidLayout = errors.New("certdesign: the layout is not valid")
	ErrInvalidPage   = errors.New("certdesign: the page cursor is not valid")
)

// Orientation values.
const (
	OrientationLandscape = "landscape"
	OrientationPortrait  = "portrait"
)

// Bounds.
const MaxLayoutElements = 200

// Audit actions.
const (
	ActionDesignCreated       = "certificate_design.created"
	ActionDesignUpdated       = "certificate_design.updated"
	ActionDesignDeleted       = "certificate_design.deleted"
	ActionDesignBackgroundSet = "certificate_design.background_set"
)

// elementKinds is the closed set of element kinds a layout may contain.
var elementKinds = map[string]bool{
	"title":     true,
	"learner":   true,
	"course":    true,
	"date":      true,
	"serial":    true,
	"signatory": true,
	"text":      true,
}

// Design is one saved certificate layout: a canvas orientation, a palette, an
// optional background image, and the positioned elements. Layout is carried as raw
// JSON — the tool owns its shape, and the domain only validates it.
type Design struct {
	ID              uuid.UUID
	Name            string
	Orientation     string
	Accent          string
	BackgroundColor string
	BackgroundKey   string
	Layout          json.RawMessage
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// NewDesign is a design to create.
type NewDesign struct {
	Name            string
	Orientation     string
	Accent          string
	BackgroundColor string
	Layout          json.RawMessage
}

// DesignPatch updates a design's editable fields in place. A nil field is left
// untouched; background is set through its own confirm step, not here.
type DesignPatch struct {
	Name            *string
	Orientation     *string
	Accent          *string
	BackgroundColor *string
	Layout          json.RawMessage
}

// element is one positioned item in a layout, validated field by field.
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

func validOrientation(o string) bool {
	return o == OrientationLandscape || o == OrientationPortrait
}

func inUnit(v float64) bool { return v >= 0 && v <= 1 }

// validateLayout ensures the raw JSON is an array of elements whose kinds are known
// and whose x/y/w are fractions in [0,1]. An empty or absent layout is the empty
// array. It returns ErrInvalidLayout on any violation.
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

func (n *NewDesign) validate() error {
	n.Name = strings.TrimSpace(n.Name)
	if n.Orientation == "" {
		n.Orientation = OrientationLandscape
	}
	if !validOrientation(n.Orientation) {
		return fmt.Errorf("%w: orientation must be landscape or portrait", ErrInvalidDesign)
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

func (p *DesignPatch) validate() error {
	if p.Name != nil {
		v := strings.TrimSpace(*p.Name)
		p.Name = &v
	}
	if p.Orientation != nil {
		if !validOrientation(*p.Orientation) {
			return fmt.Errorf("%w: orientation must be landscape or portrait", ErrInvalidDesign)
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
