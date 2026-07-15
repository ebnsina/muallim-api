// Package hifz is the madrasa Quran-memorization log: a daily record of each
// student's Sabaq, Sabqi, and Manzil. It knows nothing about HTTP, and reaches the
// student it belongs to by id, never by import.
package hifz

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Sentinels. internal/httpapi maps each to a status; add a line to errors_test.go
// in the same commit as a new one.
var (
	ErrNotFound     = errors.New("hifz: not found")
	ErrInvalidEntry = errors.New("hifz: the log entry is not valid")
	ErrInvalidPage  = errors.New("hifz: the page cursor is not valid")
)

// Kinds of daily recitation.
const (
	Sabaq  = "sabaq"  // the new lesson
	Sabqi  = "sabqi"  // recent lessons, kept fresh
	Manzil = "manzil" // the older portion, revised on a longer cycle
)

// ValidKind reports whether k is a recitation kind.
func ValidKind(k string) bool {
	switch k {
	case Sabaq, Sabqi, Manzil:
		return true
	default:
		return false
	}
}

// Ratings of how a recitation went.
const (
	Excellent = "excellent"
	Good      = "good"
	Fair      = "fair"
	Weak      = "weak"
)

func validRating(r string) bool {
	switch r {
	case Excellent, Good, Fair, Weak:
		return true
	default:
		return false
	}
}

// surahCount is the number of surahs in the Quran.
const surahCount = 114

// Entry is one recitation on one day.
type Entry struct {
	ID        uuid.UUID
	StudentID uuid.UUID
	OnDate    time.Time
	Kind      string
	Surah     int
	AyahFrom  int
	AyahTo    int
	Rating    string
	Note      string
	CreatedAt time.Time
}

// NewEntry is a recitation to log.
type NewEntry struct {
	StudentID uuid.UUID
	OnDate    time.Time
	Kind      string
	Surah     int
	AyahFrom  int
	AyahTo    int
	Rating    string
	Note      string
}

// Summary is a conservative read of a student's log: where their Sabaq stands now,
// and how much they have recited lately, by kind.
type Summary struct {
	CurrentSabaq *Entry
	Counts       map[string]int
}

func (n *NewEntry) validate() error {
	if !ValidKind(n.Kind) {
		return fmt.Errorf("%w: %q is not a recitation kind", ErrInvalidEntry, n.Kind)
	}
	if n.Surah < 1 || n.Surah > surahCount {
		return fmt.Errorf("%w: a surah is 1 to %d", ErrInvalidEntry, surahCount)
	}
	if n.AyahFrom < 1 || n.AyahTo < n.AyahFrom {
		return fmt.Errorf("%w: the ayah range is backwards", ErrInvalidEntry)
	}
	if n.Rating == "" {
		n.Rating = Good
	}
	if !validRating(n.Rating) {
		return fmt.Errorf("%w: %q is not a rating", ErrInvalidEntry, n.Rating)
	}
	return nil
}

// cursor is the keyset position in a student's log, newest first.
type cursor struct {
	OnDate time.Time `json:"d"`
	ID     uuid.UUID `json:"i"`
}

// PageParams is a request for one page of a student's log.
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

func encodeCursor(e Entry) string {
	raw, _ := json.Marshal(cursor{OnDate: e.OnDate, ID: e.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// LogPage is one page of a student's log with a keyset cursor to the next.
type LogPage struct {
	Entries    []Entry
	NextCursor string
	HasMore    bool
}
