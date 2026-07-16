package idcard

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

// Background image bounds and TTLs. A background is put in an <img>, so the
// upload list mirrors the catalog thumbnail and the serving path reads the type
// back from the key's extension — never from what the store or the uploader claims.
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
	Create(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewTemplate) (Template, error)
	List(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *cursor, limit int) ([]Template, error)
	ByID(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Template, error)
	Update(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, p TemplatePatch) (Template, error)
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

// Service holds the ID-card-designer rules and owns transaction boundaries.
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

// Create saves a new template.
func (s *Service) Create(ctx context.Context, tenantID uuid.UUID, n NewTemplate, author Author) (Template, error) {
	if err := n.validate(); err != nil {
		return Template{}, err
	}
	var t Template
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		t, err = s.repo.Create(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionTemplateCreated,
			TargetType: "id_card_template", TargetID: t.ID.String(),
			Metadata: map[string]any{"name": t.Name, "subject": t.Subject, "orientation": t.Orientation},
		})
	})
	return t, err
}

// List lists the workspace's templates, newest first, keyset-paginated.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID, p PageParams) (TemplatePage, error) {
	after, err := p.decode()
	if err != nil {
		return TemplatePage{}, err
	}
	limit := p.clamp()

	var page TemplatePage
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
		page.Templates = rows
		return nil
	})
	return page, err
}

// Get loads one template.
func (s *Service) Get(ctx context.Context, tenantID, id uuid.UUID) (Template, error) {
	var t Template
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		t, err = s.repo.ByID(ctx, tx, tenantID, id)
		return err
	})
	return t, err
}

// Update edits a template's name, subject, orientation, palette, and layout in place.
func (s *Service) Update(ctx context.Context, tenantID, id uuid.UUID, p TemplatePatch, author Author) (Template, error) {
	if err := p.validate(); err != nil {
		return Template{}, err
	}
	var t Template
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		t, err = s.repo.Update(ctx, tx, tenantID, id, p)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionTemplateUpdated,
			TargetType: "id_card_template", TargetID: t.ID.String(),
		})
	})
	return t, err
}

// Delete removes a template.
func (s *Service) Delete(ctx context.Context, tenantID, id uuid.UUID, author Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.repo.Delete(ctx, tx, tenantID, id); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionTemplateDeleted,
			TargetType: "id_card_template", TargetID: id.String(),
		})
	})
}

// PresignBackground signs a URL to upload a background image to. The author
// uploads straight to the store; ConfirmBackground records the key once the store
// has it.
func (s *Service) PresignBackground(ctx context.Context, tenantID, id uuid.UUID, contentType string, size int64) (blob.Upload, string, error) {
	ext, ok := backgroundTypes[contentType]
	if !ok {
		return blob.Upload{}, "", fmt.Errorf("%w: a background must be a PNG, JPEG, or WebP", ErrInvalidTemplate)
	}
	if size < 1 || size > MaxBackgroundBytes {
		return blob.Upload{}, "", fmt.Errorf("%w: a background must be between 1 and %d bytes", ErrInvalidTemplate, MaxBackgroundBytes)
	}

	var (
		upload blob.Upload
		key    string
	)
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tmpl, err := s.repo.ByID(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		key = fmt.Sprintf("t/%s/id-card-templates/%s/%s.%s", tenantID, tmpl.ID, uuid.New(), ext)
		upload, err = s.store.PresignPut(ctx, key, size, backgroundUploadTTL)
		return err
	})
	if err != nil {
		return blob.Upload{}, "", err
	}
	return upload, key, nil
}

// ConfirmBackground records an uploaded background once the store confirms it is
// there and within size, and deletes the object it replaces. The key must sit
// under this template's own prefix — recomputed from the template — so a key
// naming another template's or workspace's object is refused before the store is
// asked anything.
func (s *Service) ConfirmBackground(ctx context.Context, tenantID, id uuid.UUID, key string, author Author) error {
	ext := strings.TrimPrefix(path.Ext(key), ".")
	if _, ok := backgroundExtType[ext]; !ok {
		return fmt.Errorf("%w: not a background key", ErrInvalidTemplate)
	}

	var replaced string
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tmpl, err := s.repo.ByID(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}

		want := fmt.Sprintf("t/%s/id-card-templates/%s/", tenantID, tmpl.ID)
		if !strings.HasPrefix(key, want) {
			return fmt.Errorf("%w: the key is not this template's", ErrInvalidTemplate)
		}

		object, err := s.store.Head(ctx, key)
		if err != nil {
			return fmt.Errorf("%w: nothing was uploaded to that key", ErrInvalidTemplate)
		}
		if object.Size < 1 || object.Size > MaxBackgroundBytes {
			return fmt.Errorf("%w: the uploaded background is too large", ErrInvalidTemplate)
		}

		replaced, err = s.repo.SetBackground(ctx, tx, tenantID, tmpl.ID, key)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionTemplateBackgroundSet,
			TargetType: "id_card_template", TargetID: tmpl.ID.String(),
		})
	})
	if err != nil {
		return err
	}

	// The old object is now unreferenced. Deleting it is best-effort: a leaked
	// object is a cost, not a correctness bug, and the row already points at the new one.
	if replaced != "" && replaced != key {
		_ = s.store.Delete(ctx, replaced)
	}
	return nil
}

// BackgroundURL signs a short-lived URL that displays a template's background
// inline. A template with no background returns an empty string and no error —
// the absence is expected, not a failure.
func (s *Service) BackgroundURL(ctx context.Context, tenantID uuid.UUID, t Template) (string, error) {
	if t.BackgroundKey == "" {
		return "", nil
	}
	ext := strings.TrimPrefix(path.Ext(t.BackgroundKey), ".")
	contentType, ok := backgroundExtType[ext]
	if !ok {
		return "", nil
	}
	return s.store.PresignGetImage(ctx, t.BackgroundKey, contentType, backgroundViewTTL)
}
