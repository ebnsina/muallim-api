package hostel

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// cursor is the keyset position in an allocation listing, ordered newest first.
type cursor struct {
	AllocatedAt time.Time `json:"a"`
	ID          uuid.UUID `json:"i"`
}

// buildingCursor is the keyset position in a building listing, ordered by name.
type buildingCursor struct {
	Name string    `json:"n"`
	ID   uuid.UUID `json:"i"`
}

// PageParams is a request for one page of allocations.
type PageParams struct {
	Limit  int
	Cursor string
}

const (
	defaultPageLimit = 50
	maxPageLimit     = 100
)

func (p PageParams) clamp() int {
	switch {
	case p.Limit <= 0:
		return defaultPageLimit
	case p.Limit > maxPageLimit:
		return maxPageLimit
	default:
		return p.Limit
	}
}

func (p PageParams) decode() (*cursor, error) {
	if p.Cursor == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(p.Cursor)
	if err != nil {
		return nil, ErrInvalidPage
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, ErrInvalidPage
	}
	return &c, nil
}

func (p PageParams) decodeBuilding() (*buildingCursor, error) {
	if p.Cursor == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(p.Cursor)
	if err != nil {
		return nil, ErrInvalidPage
	}
	var c buildingCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, ErrInvalidPage
	}
	return &c, nil
}

func encodeCursor(a Allocation) string {
	raw, _ := json.Marshal(cursor{AllocatedAt: a.AllocatedAt, ID: a.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func encodeBuildingCursor(b Building) string {
	raw, _ := json.Marshal(buildingCursor{Name: b.Name, ID: b.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// AllocationPage is one page of allocations with a keyset cursor to the next.
type AllocationPage struct {
	Allocations []Allocation
	NextCursor  string
	HasMore     bool
}

// BuildingPage is one page of buildings with a keyset cursor to the next.
type BuildingPage struct {
	Buildings  []Building
	NextCursor string
	HasMore    bool
}
