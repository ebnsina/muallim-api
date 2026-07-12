package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
)

/*
Absent, null, and a value are three different things, and a `*T` can only hold
two of them.

This is the whole reason Optional exists. If `Sent` were ever true for a key
that was not in the body, every PATCH would erase every field it did not
mention.
*/
func TestOptionalReadsThreeStatesOffTheWire(t *testing.T) {
	t.Parallel()

	instant := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	tests := map[string]struct {
		body       string
		sent, null bool
		value      time.Time
	}{
		"absent":  {`{}`, false, false, time.Time{}},
		"null":    {`{"due_at": null}`, true, true, time.Time{}},
		"a value": {`{"due_at": "2026-07-10T12:00:00Z"}`, true, false, instant},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var body struct {
				DueAt Optional[time.Time] `json:"due_at,omitempty"`
			}
			if err := json.Unmarshal([]byte(test.body), &body); err != nil {
				t.Fatalf("unmarshalling %s: %v", test.body, err)
			}

			if body.DueAt.Sent != test.sent {
				t.Errorf("Sent = %v, want %v", body.DueAt.Sent, test.sent)
			}
			if body.DueAt.Null != test.null {
				t.Errorf("Null = %v, want %v", body.DueAt.Null, test.null)
			}
			if !body.DueAt.Value.Equal(test.value) {
				t.Errorf("Value = %v, want %v", body.DueAt.Value, test.value)
			}
		})
	}
}

/*
The generated schema says the field may be null.

That sentence is the contract. Huma's validator happens to let a `null` through
an optional field whatever the schema says, so a missing `nullable` breaks
nothing here and everything downstream: `openapi-typescript` reads `type:
string` and types the field `string | undefined`, and muallim-web then has no way to
express "remove the deadline" that will typecheck.

Asserted through a registry rather than by reading the emitted JSON, because
that is where the value comes from.
*/
func TestAnOptionalFieldIsNullableInTheSchema(t *testing.T) {
	t.Parallel()

	registry := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)

	schema := Optional[time.Time]{}.Schema(registry)
	if !schema.Nullable {
		t.Error("an Optional field is not nullable, so no client can erase it")
	}
	if schema.Type != "string" || schema.Format != "date-time" {
		t.Errorf("the underlying type was lost: type=%q format=%q", schema.Type, schema.Format)
	}

	// The registry's own schema for the type is untouched. Mutating it in place
	// would make every `time.Time` in the document nullable.
	if plain := registry.Schema(reflect.TypeOf(time.Time{}), true, ""); plain.Nullable {
		t.Error("marking one field nullable made every time.Time nullable")
	}
}

/*
The three states survive a real request.

`Optional` is only correct if Huma reaches its `UnmarshalJSON` at all — a type
whose schema generation goes wrong is a type Huma will decode as an object, and
`Sent` would then be false for a field that was plainly there.
*/
func TestAnOptionalFieldAcceptsNullThroughHuma(t *testing.T) {
	t.Parallel()

	_, api := humatest.New(t, huma.DefaultConfig("test", "1.0.0"))

	var seen Optional[time.Time]

	huma.Register(api, huma.Operation{
		OperationID: "patch", Method: http.MethodPatch, Path: "/thing",
	}, func(_ context.Context, in *struct {
		Body struct {
			DueAt Optional[time.Time] `json:"due_at,omitempty"`
		}
	}) (*struct{}, error) {
		seen = in.Body.DueAt
		return &struct{}{}, nil
	})

	for _, test := range []struct {
		name       string
		body       any
		status     int
		sent, null bool
	}{
		{"null erases", map[string]any{"due_at": nil}, http.StatusNoContent, true, true},
		{"a value sets", map[string]any{"due_at": "2026-07-10T12:00:00Z"}, http.StatusNoContent, true, false},
		{"an empty body leaves it alone", map[string]any{}, http.StatusNoContent, false, false},

		// Still a string when it is not null. Nullable widens the type by exactly one.
		{"a number is still refused", map[string]any{"due_at": 7}, http.StatusUnprocessableEntity, false, false},
	} {
		seen = Optional[time.Time]{}

		response := api.Patch("/thing", test.body)
		if response.Code != test.status {
			t.Errorf("%s: status %d, want %d (%s)", test.name, response.Code, test.status, response.Body)
			continue
		}
		if response.Code != http.StatusNoContent {
			continue
		}

		if seen.Sent != test.sent || seen.Null != test.null {
			t.Errorf("%s: Sent=%v Null=%v, want Sent=%v Null=%v", test.name, seen.Sent, seen.Null, test.sent, test.null)
		}
	}
}
