package main

import (
	"log/slog"

	"github.com/ebnsina/muallim-api/internal/platform/blob"
	"github.com/ebnsina/muallim-api/internal/platform/config"
)

// newObjectStore returns the store this deployment was given, or one that refuses.
//
// A missing bucket is not a reason not to start. A workspace with no object store
// serves every course, lesson and quiz it has; the only thing it cannot do is take
// an upload, and it says so at the moment somebody tries rather than at boot.
func newObjectStore(cfg config.Config, log *slog.Logger) (blob.Store, error) {
	if !cfg.StorageConfigured() {
		log.Warn("MUALLIM_S3_ENDPOINT is unset; assignment uploads will be refused. Run `make storage-up` for a local MinIO.")
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
