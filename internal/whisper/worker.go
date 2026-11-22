package whisper

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"time"

	"github.com/mytischi/mcpgram/internal/config"
	"github.com/mytischi/mcpgram/internal/storage"
	"github.com/mytischi/mcpgram/internal/telegram"
)

const (
	maxTranscriptionAttempts = 3
	transcriptionBatchSize   = 5
)

// FileFetcher is the Telegram API subset required to download voice/video_note payloads into memory.
type FileFetcher interface {
	GetFile(ctx context.Context, fileID string) (telegram.File, error)
	DownloadFile(ctx context.Context, filePath string, maxBytes int64) ([]byte, error)
}

// RunStats summarizes one transcription polling batch.
type RunStats struct {
	Completed int
	Failed    int
}

// Worker polls pending voice and video_note messages, downloads them in memory via Telegram Bot API,
// transcribes them via Whisper, and appends the transcript to messages.text (auto-updating FTS5).
type Worker struct {
	store       *storage.Store
	fetcher     FileFetcher
	transcriber Transcriber
	cfg         config.Config
	logger      *slog.Logger
}

// NewWorker creates a background Whisper transcription worker.
func NewWorker(store *storage.Store, fetcher FileFetcher, transcriber Transcriber, cfg config.Config, logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{
		store:       store,
		fetcher:     fetcher,
		transcriber: transcriber,
		cfg:         cfg,
		logger:      logger,
	}
}

// Enabled reports whether background Whisper transcription is active.
func (w *Worker) Enabled() bool {
	return w.cfg.WhisperMode == config.WhisperModeOpenAICompatible && w.fetcher != nil && w.transcriber != nil
}

// Run polls for pending voice notes on WHISPER_POLL_INTERVAL until ctx is canceled.
func (w *Worker) Run(ctx context.Context) error {
	if !w.Enabled() {
		w.logger.Info("whisper transcription disabled (WHISPER_MODE=off)")
		<-ctx.Done()
		return ctx.Err()
	}

	w.processOnce(ctx)

	ticker := time.NewTicker(w.cfg.WhisperPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			w.processOnce(ctx)
		}
	}
}

func (w *Worker) processOnce(ctx context.Context) {
	stats, err := w.ProcessPending(ctx)
	if err != nil {
		w.logger.Error("whisper polling pass failed", "error", err)
		return
	}
	if stats.Completed > 0 || stats.Failed > 0 {
		w.logger.Info("whisper polling pass complete",
			"completed", stats.Completed,
			"failed", stats.Failed,
		)
	}
}

// ProcessPending fetches up to transcriptionBatchSize pending voice/video_note messages and transcribes them.
func (w *Worker) ProcessPending(ctx context.Context) (RunStats, error) {
	if !w.Enabled() {
		return RunStats{}, fmt.Errorf("whisper transcription is disabled (set WHISPER_MODE=openai_compatible)")
	}

	items, err := w.store.ListPendingTranscriptions(ctx, maxTranscriptionAttempts, transcriptionBatchSize)
	if err != nil {
		return RunStats{}, err
	}

	var stats RunStats
	for _, item := range items {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}

		transcript, err := w.transcribeItem(ctx, item)
		if err != nil {
			stats.Failed++
			w.logger.Warn("voice transcription failed",
				"workspace_chat_id", item.WorkspaceChatID,
				"message_id", item.TelegramMessageID,
				"attempt", item.Attempts+1,
				"error", err,
			)
			_ = w.store.FailTranscription(ctx, item, err.Error())
			continue
		}

		if err := w.store.CompleteTranscription(ctx, item, transcript); err != nil {
			stats.Failed++
			w.logger.Error("save voice transcript failed", "error", err)
			continue
		}
		stats.Completed++
	}
	return stats, nil
}

func (w *Worker) transcribeItem(ctx context.Context, item storage.PendingTranscription) (string, error) {
	fileInfo, err := w.fetcher.GetFile(ctx, item.FileID)
	if err != nil {
		return "", fmt.Errorf("telegram getFile: %w", err)
	}
	if fileInfo.FileSize > 0 && fileInfo.FileSize > w.cfg.WhisperMaxFileBytes {
		return "", fmt.Errorf("file size %d exceeds limit %d", fileInfo.FileSize, w.cfg.WhisperMaxFileBytes)
	}

	data, err := w.fetcher.DownloadFile(ctx, fileInfo.FilePath, w.cfg.WhisperMaxFileBytes)
	if err != nil {
		return "", fmt.Errorf("telegram download file: %w", err)
	}

	filename := item.FileName
	if filename == "" && fileInfo.FilePath != "" {
		filename = path.Base(fileInfo.FilePath)
	}
	if filename == "" {
		if item.MediaType == "video_note" {
			filename = "video_note.mp4"
		} else {
			filename = "voice.ogg"
		}
	}

	return w.transcriber.Transcribe(ctx, filename, data)
}
