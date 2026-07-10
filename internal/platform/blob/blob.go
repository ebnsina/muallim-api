// Package blob stores bytes that are too large, too numerous, or too untrusted to
// live in Postgres.
//
// Nothing here ever holds a file. A browser uploads straight to the object store
// with a URL this package signed, and downloads the same way. The API process
// never sees the bytes, which is the point: a two-hundred-megabyte video passing
// through a Go handler is a handler that cannot serve anything else while it does.
//
// It knows nothing about assignments, or courses, or who is allowed to read what.
// That is the caller's problem, and the caller is the only one who can answer it.
package blob

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// ErrNotFound means no object lives at that key.
var ErrNotFound = errors.New("blob: no object at that key")

// Upload is everything a browser needs to PUT one object, and nothing else.
type Upload struct {
	URL    string
	Method string

	// Headers must be sent verbatim. They are part of what was signed, so a request
	// that omits or alters one is a request the store refuses.
	Headers http.Header

	ExpiresAt time.Time
}

// Object is what the store knows about a key without fetching it.
type Object struct {
	Size        int64
	ContentType string
	ETag        string
}

// Store signs URLs, and answers questions about keys.
//
// Declared here rather than in a domain package because two domains will want it,
// and neither should have to know that the other exists.
type Store interface {
	// PresignPut returns a URL that accepts exactly one object of exactly `size`
	// bytes at `key`, until it expires.
	//
	// The length is signed. A client that sends more, or fewer, is refused by the
	// store — which is the only place a size limit can be enforced, because the API
	// never sees the body.
	//
	// The content type is not signed, and deliberately. A browser decides what to
	// put in that header, so signing it buys a mismatch when a browser and a Go
	// program disagree about the casing of `image/JPEG`, and buys no security at
	// all: the type was always the client's word. What keeps an uploaded file from
	// executing is PresignGet, which never serves anything inline.
	PresignPut(ctx context.Context, key string, size int64, ttl time.Duration) (Upload, error)

	// PresignGet returns a URL that downloads the object, once, until it expires.
	//
	// The response is forced to `attachment`, under the name the caller supplies,
	// with a content type of `application/octet-stream`. A submitted file is bytes
	// a stranger chose; a browser that renders it is a browser running a stranger's
	// HTML with the object store's origin, and every content-type check ever written
	// is a check on a header that same stranger set.
	PresignGet(ctx context.Context, key, downloadName string, ttl time.Duration) (string, error)

	// Head reports what is actually at a key, which is not necessarily what a
	// client said it would put there.
	Head(ctx context.Context, key string) (Object, error)

	// Delete removes an object. Deleting a key that is already gone is not an error:
	// the caller wanted it gone, and it is.
	Delete(ctx context.Context, key string) error
}

// ErrNotConfigured means this deployment has no object store.
var ErrNotConfigured = errors.New("blob: no object store is configured")

// Unconfigured is a Store that refuses everything, for a deployment that has not
// been given a bucket.
//
// The alternative was to make the store optional everywhere and check for nil at
// each call site, which is the same check written six times and forgotten once.
// A deployment with no bucket cannot accept an upload, and saying so plainly at
// the moment somebody tries is better than a signed URL pointing at nowhere.
type Unconfigured struct{}

func (Unconfigured) PresignPut(context.Context, string, int64, time.Duration) (Upload, error) {
	return Upload{}, ErrNotConfigured
}

func (Unconfigured) PresignGet(context.Context, string, string, time.Duration) (string, error) {
	return "", ErrNotConfigured
}

func (Unconfigured) Head(context.Context, string) (Object, error) { return Object{}, ErrNotConfigured }
func (Unconfigured) Delete(context.Context, string) error         { return ErrNotConfigured }
