package exams

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// cursor is the keyset position in the exam list: exams are ordered newest first,
// so a page resumes at the row just older than this one.
type cursor struct {
	CreatedAt time.Time `json:"c"`
	ID        uuid.UUID `json:"i"`
}

// PageParams is a request for one page of exams.
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

func encodeCursor(e Exam) string {
	raw, _ := json.Marshal(cursor{CreatedAt: e.CreatedAt, ID: e.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// ExamPage is one page of exams with a keyset cursor to the next.
type ExamPage struct {
	Exams      []Exam
	NextCursor string
	HasMore    bool
}
