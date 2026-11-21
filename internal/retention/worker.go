package retention

import (
	"context"
	"log/slog"
	"time"

	"github.com/mytischi/mcpgram/internal/storage"
)

// Worker periodically deletes messages older than RetentionDays.
type Worker struct {
	store         *storage.Store
	retentionDays int
	interval      time.Duration
	logger        *slog.Logger
}

// NewWorker creates a retention cleanup worker.
func NewWorker(store *storage.Store, retentionDays int, interval time.Duration, logger *slog.Logger) *Worker {
	if interval <= 0 {
		interval = time.Hour
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{
		store:         store,
		retentionDays: retentionDays,
		interval:      interval,
		logger:        logger,
	}
}

// Run executes an initial purge and repeats on interval until ctx is canceled.
func (w *Worker) Run(ctx context.Context) error {
	if w.retentionDays <= 0 {
		w.logger.Info("retention cleanup disabled (RETENTION_DAYS=0)")
		<-ctx.Done()
		return ctx.Err()
	}

	w.purgeOnce(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			w.purgeOnce(ctx)
		}
	}
}

func (w *Worker) purgeOnce(ctx context.Context) {
	deleted, err := w.store.PurgeExpiredMessages(ctx, w.retentionDays, time.Now().UTC())
	if err != nil {
		w.logger.Error("retention purge failed", "error", err)
		return
	}
	if deleted > 0 {
		w.logger.Info("purged expired messages",
			"deleted", deleted,
			"retention_days", w.retentionDays,
		)
	}
}
