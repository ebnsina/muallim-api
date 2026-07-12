package httpapi

import (
	"bytes"
	"encoding/json"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
)

// Optional is a field a PATCH may leave alone, set, or erase.
//
// A `*T` has two states and JSON has three. An absent `due_at` means "keep the
// deadline you have", a string means "make it this", and `null` means "there is
// no deadline any more" — and a pointer collapses the first and the last into
// the same nil. An author who could set a deadline but never take one off would
// have to delete the assignment, and every submission under it, to undo a typo.
//
// The three states are read off the wire here and handed to the domain as two
// plain fields, so no domain package has to know that JSON has a null.
type Optional[T any] struct {
	// Sent is true when the field appeared in the body at all.
	Sent bool
	// Null is true when it appeared as `null`.
	Null bool
	// Value is meaningful only when Sent is true and Null is false.
	Value T
}

// UnmarshalJSON is only called for a key that is present, which is what makes
// `Sent` true. An absent key leaves the zero value, and the zero value means
// "unchanged" — a patch nobody filled in changes nothing.
func (o *Optional[T]) UnmarshalJSON(b []byte) error {
	o.Sent = true

	if bytes.Equal(b, []byte("null")) {
		o.Null = true
		return nil
	}

	return json.Unmarshal(b, &o.Value)
}

// MarshalJSON keeps a round trip honest, and keeps `Optional` usable in a
// response should one ever need it. An unsent field marshals as null rather than
// disappearing, because a struct field cannot omit itself.
func (o Optional[T]) MarshalJSON() ([]byte, error) {
	if !o.Sent || o.Null {
		return []byte("null"), nil
	}
	return json.Marshal(o.Value)
}

// Schema is the underlying type's schema, made nullable.
//
// Huma validates the request body against this before it unmarshals anything, so
// without the nullable flag a `null` is a 422 and `UnmarshalJSON` never runs.
// The generated OpenAPI document says `type: [string, "null"]`, which is what
// tells muallim-web — and every other client — that erasing the field is allowed.
func (o Optional[T]) Schema(r huma.Registry) *huma.Schema {
	// Copied, not mutated in place. The registry hands back the schema it holds for
	// this type, and marking that one nullable would make every `time.Time` in the
	// document nullable along with it.
	nullable := *r.Schema(reflect.TypeOf(o.Value), true, "")
	nullable.Nullable = true
	return &nullable
}
