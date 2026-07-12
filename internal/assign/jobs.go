package assign

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/ebnsina/muallim-api/internal/platform/blob"
)

// DeleteObjectsArgs removes objects whose rows have gone.
//
// It carries the tenant only for the audit trail and the logs; an object key
// already names its tenant, and the store has never heard of one.
type DeleteObjectsArgs struct {
	TenantID uuid.UUID `json:"tenant_id"`
	Keys     []string  `json:"keys"`
}

// Kind is stored in every river_job row, so it is a name and not a label.
func (DeleteObjectsArgs) Kind() string { return "assign.delete_objects" }

// InsertOpts retries. An object nobody deletes is an object somebody pays for
// every month until the end of time.
func (DeleteObjectsArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 10}
}

// DeleteObjectsWorker deletes them.
type DeleteObjectsWorker struct {
	river.WorkerDefaults[DeleteObjectsArgs]

	store blob.Store
}

// NewDeleteObjectsWorker returns a worker, refusing one it cannot run.
func NewDeleteObjectsWorker(store blob.Store) (*DeleteObjectsWorker, error) {
	if store == nil {
		return nil, errors.New("assign: the deletion worker needs an object store")
	}
	return &DeleteObjectsWorker{store: store}, nil
}

// Work deletes each object.
//
// Idempotent: deleting a key that is already gone is not an error, so a retry
// after a partial failure finishes the job rather than failing on the first key
// the last attempt succeeded at.
func (w *DeleteObjectsWorker) Work(ctx context.Context, job *river.Job[DeleteObjectsArgs]) error {
	for _, key := range job.Args.Keys {
		if err := w.store.Delete(ctx, key); err != nil {
			return fmt.Errorf("assign: delete %s: %w", key, err)
		}
	}
	return nil
}

// RiverEnqueuer adapts a River client to Enqueuer.
//
// It lives here rather than in cmd/ because the job's arguments do, and a caller
// forced to construct them itself could construct them wrongly.
type RiverEnqueuer struct {
	client *river.Client[pgx.Tx]
}

// NewRiverEnqueuer returns an Enqueuer backed by River.
func NewRiverEnqueuer(client *river.Client[pgx.Tx]) (*RiverEnqueuer, error) {
	if client == nil {
		return nil, errors.New("assign: the river enqueuer needs a client")
	}
	return &RiverEnqueuer{client: client}, nil
}

// DeleteObjects queues the deletion on the caller's transaction, so the row and
// the job commit together — or neither does.
func (e *RiverEnqueuer) DeleteObjects(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, keys []string) error {
	if len(keys) == 0 {
		return nil
	}

	args := DeleteObjectsArgs{TenantID: tenantID, Keys: keys}
	if _, err := e.client.InsertTx(ctx, tx, args, nil); err != nil {
		return fmt.Errorf("assign: enqueue the deletion of %d objects: %w", len(keys), err)
	}
	return nil
}
