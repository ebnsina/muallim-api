package certdesign

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/blob"
	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Background image bounds and TTLs. A background is put in an <img>, so the upload
// list mirrors the catalog thumbnail and the serving path reads the type back from
// the key's extension — never from what the store or the uploader claims.
const (
	MaxBackgroundBytes = 8 << 20

	backgroundUploadTTL = 15 * time.Minute
	backgroundViewTTL   = 5 * time.Minute
)

// backgroundTypes maps an accepted upload content type to the extension its key
// carries. Nothing outside this list may be uploaded.
var backgroundTypes = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/webp": "webp",
}

// backgroundExtType is backgroundTypes inverted: extension back to the type to serve.
var backgroundExtType = map[string]string{
	"png":  "image/png",
	"jpg":  "image/jpeg",
	"webp": "image/webp",
}

// Repository is the persistence contract, declared by its consumer. Every method
// takes a transaction, never the pool: the tenant is bound transaction-locally.
type Repository interface {
	Create(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewDesign) (Design, error)
	List(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *cursor, limit int) ([]Design, error)
	ByID(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Design, error)
	Update(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, p DesignPatch) (Design, error)
	SetBackground(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, key string) (string, error)
	Delete(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error
}

// AuditRecorder writes an audit line in the transaction of the thing it describes.
type AuditRecorder interface {
	Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e AuditEntry) error
}

// AuditEntry is one line of the audit log.
type AuditEntry struct {
	ActorID    *uuid.UUID
	Action     string
	TargetType string
	TargetID   string
	Metadata   map[string]any
}

// Author identifies who did something, for the audit trail.
type Author struct {
	UserID uuid.UUID
}

// Service holds the certificate-designer rules and owns transaction boundaries.
type Service struct {
	db    *database.DB
	repo  Repository
	store blob.Store
	audit AuditRecorder
}

// NewService returns a Service. The store holds background images; a nil-behaving
// (Unconfigured) store refuses those uploads.
func NewService(db *database.DB, repo Repository, store blob.Store, recorder AuditRecorder) *Service {
	return &Service{db: db, repo: repo, store: store, audit: recorder}
}

// Create saves a new design.
func (s *Service) Create(ctx context.Context, tenantID uuid.UUID, n NewDesign, author Author) (Design, error) {
	if err := n.validate(); err != nil {
		return Design{}, err
	}
	var d Design
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = s.repo.Create(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionDesignCreated,
			TargetType: "certificate_design", TargetID: d.ID.String(),
			Metadata: map[string]any{"name": d.Name, "orientation": d.Orientation},
		})
	})
	return d, err
}

// List lists the workspace's designs, newest first, keyset-paginated.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID, p PageParams) (DesignPage, error) {
	after, err := p.decode()
	if err != nil {
		return DesignPage{}, err
	}
	limit := p.clamp()

	var page DesignPage
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.repo.List(ctx, tx, tenantID, after, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			page.HasMore = true
			page.NextCursor = encodeCursor(rows[limit-1])
			rows = rows[:limit]
		}
		page.Designs = rows
		return nil
	})
	return page, err
}

// Get loads one design.
func (s *Service) Get(ctx context.Context, tenantID, id uuid.UUID) (Design, error) {
	var d Design
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = s.repo.ByID(ctx, tx, tenantID, id)
		return err
	})
	return d, err
}

// Update edits a design's name, orientation, palette, and layout in place.
func (s *Service) Update(ctx context.Context, tenantID, id uuid.UUID, p DesignPatch, author Author) (Design, error) {
	if err := p.validate(); err != nil {
		return Design{}, err
	}
	var d Design
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = s.repo.Update(ctx, tx, tenantID, id, p)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionDesignUpdated,
			TargetType: "certificate_design", TargetID: d.ID.String(),
		})
	})
	return d, err
}

// Delete removes a design.
func (s *Service) Delete(ctx context.Context, tenantID, id uuid.UUID, author Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.repo.Delete(ctx, tx, tenantID, id); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionDesignDeleted,
			TargetType: "certificate_design", TargetID: id.String(),
		})
	})
}

// PresignBackground signs a URL to upload a background image to. The author uploads
// straight to the store; ConfirmBackground records the key once the store has it.
func (s *Service) PresignBackground(ctx context.Context, tenantID, id uuid.UUID, contentType string, size int64) (blob.Upload, string, error) {
	ext, ok := backgroundTypes[contentType]
	if !ok {
		return blob.Upload{}, "", fmt.Errorf("%w: a background must be a PNG, JPEG, or WebP", ErrInvalidDesign)
	}
	if size < 1 || size > MaxBackgroundBytes {
		return blob.Upload{}, "", fmt.Errorf("%w: a background must be between 1 and %d bytes", ErrInvalidDesign, MaxBackgroundBytes)
	}

	var (
		upload blob.Upload
		key    string
	)
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		design, err := s.repo.ByID(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		key = fmt.Sprintf("t/%s/certificate-designs/%s/%s.%s", tenantID, design.ID, uuid.New(), ext)
		upload, err = s.store.PresignPut(ctx, key, size, backgroundUploadTTL)
		return err
	})
	if err != nil {
		return blob.Upload{}, "", err
	}
	return upload, key, nil
}

// ConfirmBackground records an uploaded background once the store confirms it is
// there and within size, and deletes the object it replaces. The key must sit under
// this design's own prefix — recomputed from the design — so a key naming another
// design's or workspace's object is refused before the store is asked anything.
func (s *Service) ConfirmBackground(ctx context.Context, tenantID, id uuid.UUID, key string, author Author) error {
	ext := strings.TrimPrefix(path.Ext(key), ".")
	if _, ok := backgroundExtType[ext]; !ok {
		return fmt.Errorf("%w: not a background key", ErrInvalidDesign)
	}

	var replaced string
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		design, err := s.repo.ByID(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}

		want := fmt.Sprintf("t/%s/certificate-designs/%s/", tenantID, design.ID)
		if !strings.HasPrefix(key, want) {
			return fmt.Errorf("%w: the key is not this design's", ErrInvalidDesign)
		}

		object, err := s.store.Head(ctx, key)
		if err != nil {
			return fmt.Errorf("%w: nothing was uploaded to that key", ErrInvalidDesign)
		}
		if object.Size < 1 || object.Size > MaxBackgroundBytes {
			return fmt.Errorf("%w: the uploaded background is too large", ErrInvalidDesign)
		}

		replaced, err = s.repo.SetBackground(ctx, tx, tenantID, design.ID, key)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionDesignBackgroundSet,
			TargetType: "certificate_design", TargetID: design.ID.String(),
		})
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

// BackgroundURL signs a short-lived URL that displays a design's background inline.
// A design with no background returns an empty string and no error — the absence is
// expected, not a failure.
func (s *Service) BackgroundURL(ctx context.Context, tenantID uuid.UUID, d Design) (string, error) {
	if d.BackgroundKey == "" {
		return "", nil
	}
	ext := strings.TrimPrefix(path.Ext(d.BackgroundKey), ".")
	contentType, ok := backgroundExtType[ext]
	if !ok {
		return "", nil
	}
	return s.store.PresignGetImage(ctx, d.BackgroundKey, contentType, backgroundViewTTL)
}
