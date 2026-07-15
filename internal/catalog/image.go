package catalog

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/blob"
)

// A thumbnail is small, and its type is one of a short list. The type decides the
// key's extension, which is how the serving path knows what to declare it as
// without a second column.
const (
	MaxImageBytes = 5 << 20

	imageUploadTTL = 15 * time.Minute
	imageViewTTL   = 5 * time.Minute
)

// imageTypes maps an accepted upload content type to the extension its key carries.
// Nothing outside this list may be uploaded, and the serving path reads the type
// back from the extension — never from what the store or the uploader claims.
var imageTypes = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/webp": "webp",
}

// imageExtType is imageTypes inverted: extension back to the content type to serve.
var imageExtType = map[string]string{
	"png":  "image/png",
	"jpg":  "image/jpeg",
	"webp": "image/webp",
}

// PresignImage signs a URL to upload a course thumbnail to. The author uploads
// straight to the store; ConfirmImage records the key once the store has the bytes.
func (s *Service) PresignImage(ctx context.Context, tenantID uuid.UUID, slug, contentType string, size int64) (blob.Upload, string, error) {
	ext, ok := imageTypes[contentType]
	if !ok {
		return blob.Upload{}, "", fmt.Errorf("%w: a thumbnail must be a PNG, JPEG, or WebP", ErrInvalidImage)
	}
	if size < 1 || size > MaxImageBytes {
		return blob.Upload{}, "", fmt.Errorf("%w: a thumbnail must be between 1 and %d bytes", ErrInvalidImage, MaxImageBytes)
	}

	var (
		upload blob.Upload
		key    string
	)
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Drafts included: an author sets a thumbnail before publishing.
		course, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug, true)
		if err != nil {
			return err
		}

		key = fmt.Sprintf("t/%s/courses/%s/%s.%s", tenantID, course.ID, uuid.New(), ext)
		upload, err = s.store.PresignPut(ctx, key, size, imageUploadTTL)
		return err
	})
	if err != nil {
		return blob.Upload{}, "", err
	}
	return upload, key, nil
}

// ConfirmImage records an uploaded thumbnail once the store confirms it is there
// and within size, and deletes the object it replaces.
//
// The key must sit under this course's own prefix — recomputed from the course, so
// a key naming another course's or workspace's object is refused before the store
// is asked anything.
func (s *Service) ConfirmImage(ctx context.Context, tenantID uuid.UUID, slug, key string) error {
	ext := strings.TrimPrefix(path.Ext(key), ".")
	if _, ok := imageExtType[ext]; !ok {
		return fmt.Errorf("%w: not a thumbnail key", ErrInvalidImage)
	}

	var replaced string
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		course, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug, true)
		if err != nil {
			return err
		}

		want := fmt.Sprintf("t/%s/courses/%s/", tenantID, course.ID)
		if !strings.HasPrefix(key, want) {
			return fmt.Errorf("%w: the key is not this course's", ErrInvalidImage)
		}

		object, err := s.store.Head(ctx, key)
		if err != nil {
			return fmt.Errorf("%w: nothing was uploaded to that key", ErrInvalidImage)
		}
		if object.Size < 1 || object.Size > MaxImageBytes {
			return fmt.Errorf("%w: the uploaded thumbnail is too large", ErrInvalidImage)
		}

		replaced, err = s.authoring.SetCourseImage(ctx, tx, tenantID, course.ID, key)
		return err
	})
	if err != nil {
		return err
	}

	// The old object is now unreferenced. Deleting it is best-effort: a leaked object
	// is a cost, not a correctness bug, and the row already points at the new one.
	if replaced != "" && replaced != key {
		_ = s.store.Delete(ctx, replaced)
	}
	return nil
}

// ImageURL signs a short-lived URL that displays a course's thumbnail inline.
//
// includeDrafts must come from an authorisation decision: a draft's thumbnail is no
// more visible than the draft. A course with no image is a 404, not an empty URL.
func (s *Service) ImageURL(ctx context.Context, tenantID uuid.UUID, slug string, includeDrafts bool) (string, error) {
	var url string
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		course, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug, includeDrafts)
		if err != nil {
			return err
		}
		if course.ImageKey == "" {
			return ErrNoImage
		}

		ext := strings.TrimPrefix(path.Ext(course.ImageKey), ".")
		contentType, ok := imageExtType[ext]
		if !ok {
			return fmt.Errorf("%w: unknown image type", ErrInvalidImage)
		}

		url, err = s.store.PresignGetImage(ctx, course.ImageKey, contentType, imageViewTTL)
		return err
	})
	if err != nil {
		return "", err
	}
	return url, nil
}
