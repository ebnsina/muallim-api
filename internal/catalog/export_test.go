package catalog

import (
	"time"

	"github.com/google/uuid"
)

// Statements exposed to the package's external test, so that a query plan is
// asserted against the SQL this package actually issues rather than a copy of it
// that can drift.
var (
	ListPublishedCoursesSQL = listPublishedCoursesSQL
	ListAllCoursesSQL       = listAllCoursesSQL
)

// DecodeCursorForTest opens the opaque cursor so a test can feed its components
// back into an EXPLAIN. Clients must not do this, which is why it lives in a
// file the compiler leaves out of the package.
func DecodeCursorForTest(encoded string) (time.Time, uuid.UUID, error) {
	c, err := decodeCursor(encoded)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	return c.CreatedAt, c.ID, nil
}
