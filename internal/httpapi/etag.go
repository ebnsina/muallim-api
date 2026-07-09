package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"
)

// maxETagBody bounds how much of a response we will buffer to hash. Beyond it the
// response streams through unhashed: holding an unbounded body in memory to save
// a client some bandwidth is a trade in the wrong direction.
const maxETagBody = 1 << 20 // 1 MiB

// etag adds an entity tag to cacheable responses and answers a matching
// If-None-Match with 304 Not Modified.
//
// The saving is bandwidth and parse time, not database work: the handler has
// already run by the time we compare. That is the honest trade for a hash of the
// body, and it is why hot read paths should also carry a Cache-Control max-age,
// so a fresh client does not revalidate at all.
//
// Only 200 responses to GET and HEAD are tagged. A tagged 404 would be a cache
// key for an absence, and a tagged POST response is meaningless.
func etag(log *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				next.ServeHTTP(w, r)
				return
			}

			bw := &bufferedWriter{ResponseWriter: w}
			next.ServeHTTP(bw, r)

			if bw.status() != http.StatusOK || bw.overflowed {
				bw.flush()
				return
			}

			tag := computeETag(bw.body.Bytes())
			bw.Header().Set("ETag", tag)

			if matches(r.Header.Get("If-None-Match"), tag) {
				// A 304 must carry no body, and RFC 9110 requires it to repeat the
				// headers a 200 would have sent for caching purposes — ETag and
				// Cache-Control are already on the writer.
				bw.ResponseWriter.WriteHeader(http.StatusNotModified)
				bw.discarded = true
				return
			}

			bw.flush()
		})
	}
}

// computeETag returns a strong entity tag over the response body.
//
// SHA-256 truncated to 128 bits: an ETag is a cache key, not a security
// boundary, and 16 bytes make collision between two bodies of the same resource
// implausible.
func computeETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + base64.RawURLEncoding.EncodeToString(sum[:16]) + `"`
}

// matches reports whether the If-None-Match header selects tag.
//
// The header is a comma-separated list, may be "*", and entries may be weak
// ("W/"). We compare on the opaque value so a weak validator still matches the
// strong tag we issued for the same bytes.
func matches(header, tag string) bool {
	if header == "" {
		return false
	}
	if strings.TrimSpace(header) == "*" {
		return true
	}

	want := strings.TrimPrefix(tag, "W/")
	for candidate := range strings.SplitSeq(header, ",") {
		if strings.TrimPrefix(strings.TrimSpace(candidate), "W/") == want {
			return true
		}
	}
	return false
}

// bufferedWriter accumulates a response so it can be hashed before it is sent.
type bufferedWriter struct {
	http.ResponseWriter

	code       int
	wrote      bool
	overflowed bool
	discarded  bool
	body       bytes.Buffer
}

func (b *bufferedWriter) WriteHeader(code int) {
	if b.wrote {
		return
	}
	b.code = code
	b.wrote = true

	// A response we will not tag has no reason to be buffered.
	if code != http.StatusOK {
		b.overflowed = true
		b.ResponseWriter.WriteHeader(code)
	}
}

func (b *bufferedWriter) Write(p []byte) (int, error) {
	if !b.wrote {
		b.WriteHeader(http.StatusOK)
	}
	if b.overflowed {
		return b.ResponseWriter.Write(p)
	}

	if b.body.Len()+len(p) > maxETagBody {
		// Too large to hash. Commit what we have and stream the rest.
		b.overflowed = true
		b.ResponseWriter.WriteHeader(b.code)
		if _, err := b.ResponseWriter.Write(b.body.Bytes()); err != nil {
			return 0, err
		}
		b.body.Reset()
		return b.ResponseWriter.Write(p)
	}

	return b.body.Write(p)
}

func (b *bufferedWriter) flush() {
	if b.discarded || b.overflowed {
		return
	}
	if !b.wrote {
		return
	}
	b.ResponseWriter.WriteHeader(b.code)
	if b.body.Len() > 0 {
		if _, err := b.ResponseWriter.Write(b.body.Bytes()); err != nil {
			return
		}
	}
}

func (b *bufferedWriter) status() int {
	if b.code == 0 {
		return http.StatusOK
	}
	return b.code
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (b *bufferedWriter) Unwrap() http.ResponseWriter { return b.ResponseWriter }
