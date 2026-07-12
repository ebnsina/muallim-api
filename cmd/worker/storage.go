package main

import (
	"log/slog"

	"github.com/ebnsina/muallim-api/internal/platform/blob"
	"github.com/ebnsina/muallim-api/internal/platform/config"
)

// newObjectStore returns the store this deployment was given, or one that refuses.
//
// A missing bucket is not a reason not to start. A deployment with no store has
// no files to delete, so the job is never enqueued — and if one somehow is, it
// fails loudly rather than reporting success for work it did not do.
func newObjectStore(cfg config.Config, log *slog.Logger) (blob.Store, error) {
	if !cfg.StorageConfigured() {
		log.Warn("LMS_S3_ENDPOINT is unset; object deletions will fail. Run `make storage-up` for a local MinIO.")
		return blob.Unconfigured{}, nil
	}

	return blob.NewS3(blob.Options{
		Endpoint:  cfg.S3Endpoint,
		Bucket:    cfg.S3Bucket,
		AccessKey: cfg.S3AccessKey,
		SecretKey: cfg.S3SecretKey,
		Region:    cfg.S3Region,
		PathStyle: cfg.S3PathStyle,
	})
}
