package blob_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ebnsina/muallim-api/internal/platform/blob"
)

/*
Against a real S3 API, or not at all.

A mock of an object store tests the mock. Whether a presigned URL refuses a body
one byte too long is a question only the store can answer, and it is the whole
reason the length is signed. `make storage-up` starts MinIO; CI does the same.
*/
func testStore(t *testing.T) *blob.S3 {
	t.Helper()

	endpoint := os.Getenv("MUALLIM_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("MUALLIM_TEST_S3_ENDPOINT is not set; skipping object-store tests")
	}

	store, err := blob.NewS3(blob.Options{
		Endpoint:  endpoint,
		Bucket:    envOr("MUALLIM_TEST_S3_BUCKET", "muallim-uploads"),
		AccessKey: envOr("MUALLIM_TEST_S3_ACCESS_KEY", "muallim"),
		SecretKey: envOr("MUALLIM_TEST_S3_SECRET_KEY", "muallim-secret-key"),
		Region:    "auto",
		PathStyle: true,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return store
}

// Cleanups run after t.Context() is cancelled, so they need one of their own.
func ctxBackground() context.Context { return context.Background() }

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// put performs the upload a browser would, from the signed request.
func put(t *testing.T, upload blob.Upload, body []byte) int {
	t.Helper()

	request, err := http.NewRequest(upload.Method, upload.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build the upload request: %v", err)
	}

	for name, values := range upload.Headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}

	// net/http will not send a Content-Length header for you when it came out of a
	// map. The signature covers it, so it must be on the wire.
	if declared := request.Header.Get("Content-Length"); declared != "" {
		request.ContentLength, _ = strconv.ParseInt(declared, 10, 64)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer response.Body.Close()

	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

func key(t *testing.T) string {
	t.Helper()
	return "test/" + uuid.NewString()
}

func TestAnObjectGoesUpAndComesDown(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	ctx := t.Context()
	name := key(t)
	body := []byte("the method is the thing that survives the problem")

	upload, err := store.PresignPut(ctx, name, int64(len(body)), time.Minute)
	if err != nil {
		t.Fatalf("presign put: %v", err)
	}
	if upload.Method != http.MethodPut {
		t.Errorf("method = %q, want PUT", upload.Method)
	}
	if upload.ExpiresAt.Before(time.Now()) {
		t.Error("the upload has already expired")
	}

	if status := put(t, upload, body); status != http.StatusOK {
		t.Fatalf("upload returned %d", status)
	}
	t.Cleanup(func() { _ = store.Delete(ctxBackground(), name) })

	object, err := store.Head(ctx, name)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if object.Size != int64(len(body)) {
		t.Errorf("size = %d, want %d", object.Size, len(body))
	}

	// The download is signed, and comes back as an attachment under the name asked
	// for — never inline, whatever the uploader claimed the type was.
	download, err := store.PresignGet(ctx, name, "Fatima's answer.txt", time.Minute)
	if err != nil {
		t.Fatalf("presign get: %v", err)
	}

	response, err := http.Get(download)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer response.Body.Close()

	got, _ := io.ReadAll(response.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("downloaded %q", got)
	}

	if disposition := response.Header.Get("Content-Disposition"); !strings.HasPrefix(disposition, "attachment") {
		t.Errorf("Content-Disposition = %q, want an attachment", disposition)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "application/octet-stream" {
		t.Errorf("Content-Type = %q; a submitted file is never served as itself", contentType)
	}
}

/*
The size limit, and the only place it can be enforced.

The API never sees the bytes, so nothing on our side can count them. `Content-Length`
is part of what SigV4 signed, so a client that wants to upload more than it was
granted has to declare a different length — and the signature no longer matches.

Note what this test does *not* do. It cannot send eleven bytes under a
`Content-Length: 10`, because Go's own http client refuses to: HTTP has no way
to express it, and the store would read ten bytes and ignore the rest. The
interesting attack is the one a client can actually mount, which is to change
the number.
*/
func TestASignedLengthIsTheSizeLimit(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	name := key(t)

	// Signed for ten bytes.
	upload, err := store.PresignPut(t.Context(), name, 10, time.Minute)
	if err != nil {
		t.Fatalf("presign put: %v", err)
	}
	if upload.Headers.Get("Content-Length") != "10" {
		t.Fatalf("the length was not signed: %v", upload.Headers)
	}

	// Eleven bytes, honestly declared. The signature covered ten.
	tampered := blob.Upload{URL: upload.URL, Method: upload.Method, Headers: http.Header{}}
	for name, values := range upload.Headers {
		tampered.Headers[name] = values
	}
	tampered.Headers.Set("Content-Length", "11")

	if status := put(t, tampered, bytes.Repeat([]byte("x"), 11)); status == http.StatusOK {
		t.Error("the store accepted eleven bytes against a signature for ten")
		_ = store.Delete(ctxBackground(), name)
	}

	// And exactly ten is accepted.
	if status := put(t, upload, bytes.Repeat([]byte("x"), 10)); status != http.StatusOK {
		t.Fatalf("the store refused exactly the signed length: %d", status)
	}
	t.Cleanup(func() { _ = store.Delete(ctxBackground(), name) })
}

func TestHeadingAKeyThatIsNotThere(t *testing.T) {
	t.Parallel()

	store := testStore(t)

	if _, err := store.Head(t.Context(), key(t)); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("head of a missing key: %v, want ErrNotFound", err)
	}
}

// Deleting is what the caller wanted, and after it the object is gone. That it was
// already gone is not news.
func TestDeletingIsIdempotent(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	name := key(t)

	if err := store.Delete(t.Context(), name); err != nil {
		t.Errorf("deleting a key that was never there: %v", err)
	}

	upload, err := store.PresignPut(t.Context(), name, 4, time.Minute)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	if status := put(t, upload, []byte("gone")); status != http.StatusOK {
		t.Fatalf("upload: %d", status)
	}

	if err := store.Delete(t.Context(), name); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Head(t.Context(), name); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("the object survived its deletion: %v", err)
	}
	if err := store.Delete(t.Context(), name); err != nil {
		t.Errorf("deleting twice: %v", err)
	}
}

// A signed URL is a bearer token with a clock on it.
func TestAnExpiredSignatureIsRefused(t *testing.T) {
	t.Parallel()

	store := testStore(t)

	// One second, and then wait it out. The store compares against its own clock,
	// so this is the shortest honest way to ask the question.
	upload, err := store.PresignPut(t.Context(), key(t), 4, time.Second)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}

	time.Sleep(2 * time.Second)

	if status := put(t, upload, []byte("late")); status == http.StatusOK {
		t.Fatal("the store accepted an expired signature")
	}
}
