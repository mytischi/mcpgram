package semantic

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mytischi/mcpgram/internal/config"
	"github.com/mytischi/mcpgram/internal/storage"
)

const embedBatchSize = 16

// IndexStats summarizes one semantic indexing pass.
type IndexStats struct {
	ChatsScanned int
	Indexed      int
	Skipped      int
	Failed       int
}

// Indexer periodically groups new/updated messages into dialog chunks and stores their embeddings.
type Indexer struct {
	store    *storage.Store
	embedder Embedder
	cfg      config.Config
	logger   *slog.Logger
}

// NewIndexer creates a background semantic chunk indexer.
func NewIndexer(store *storage.Store, embedder Embedder, cfg config.Config, logger *slog.Logger) *Indexer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Indexer{
		store:    store,
		embedder: embedder,
		cfg:      cfg,
		logger:   logger,
	}
}

// Enabled reports whether semantic indexing is active.
func (idx *Indexer) Enabled() bool {
	return idx.cfg.EmbeddingMode == config.EmbeddingModeOpenAICompatible && idx.embedder != nil
}

// Run executes an initial indexing pass and repeats every EMBEDDING_POLL_INTERVAL until ctx is canceled.
func (idx *Indexer) Run(ctx context.Context) error {
	if !idx.Enabled() {
		idx.logger.Info("semantic indexing disabled (EMBEDDING_MODE=off)")
		<-ctx.Done()
		return ctx.Err()
	}

	idx.runOnce(ctx)

	ticker := time.NewTicker(idx.cfg.EmbeddingPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			idx.runOnce(ctx)
		}
	}
}

func (idx *Indexer) runOnce(ctx context.Context) {
	stats, err := idx.IndexAll(ctx)
	if err != nil {
		idx.logger.Error("semantic indexing pass failed", "error", err)
		return
	}
	if stats.Indexed > 0 || stats.Failed > 0 {
		idx.logger.Info("semantic indexing pass complete",
			"chats", stats.ChatsScanned,
			"indexed", stats.Indexed,
			"skipped", stats.Skipped,
			"failed", stats.Failed,
		)
	}
}

// IndexAll scans all tracked chats and embeds any new or modified dialog chunks idempotently.
func (idx *Indexer) IndexAll(ctx context.Context) (IndexStats, error) {
	if !idx.Enabled() {
		return IndexStats{}, fmt.Errorf("semantic indexing is disabled (set EMBEDDING_MODE=openai_compatible)")
	}

	chats, err := idx.store.ListAllChats(ctx)
	if err != nil {
		return IndexStats{}, fmt.Errorf("list chats for semantic index: %w", err)
	}

	var stats IndexStats
	model := idx.embedder.Model()

	for _, chat := range chats {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		stats.ChatsScanned++

		msgs, err := idx.store.ListChatMessagesAll(ctx, chat.WorkspaceChatID)
		if err != nil {
			stats.Failed++
			idx.logger.Error("load chat messages for indexing failed",
				"workspace_chat_id", chat.WorkspaceChatID,
				"error", err,
			)
			continue
		}

		existingHashes, err := idx.store.ListChunkHashesByChat(ctx, chat.WorkspaceChatID, model)
		if err != nil {
			stats.Failed++
			idx.logger.Error("load existing chunk hashes failed",
				"workspace_chat_id", chat.WorkspaceChatID,
				"error", err,
			)
			continue
		}

		chunks := BuildDialogChunks(msgs, idx.cfg.EmbeddingChunkWindowSec, idx.cfg.EmbeddingChunkMaxMsgs)
		var pending []DialogChunk
		for _, ch := range chunks {
			if existingHashes[ch.Key()] == ch.ContentHash {
				stats.Skipped++
				continue
			}
			pending = append(pending, ch)
		}

		for start := 0; start < len(pending); start += embedBatchSize {
			end := start + embedBatchSize
			if end > len(pending) {
				end = len(pending)
			}
			batch := pending[start:end]
			texts := make([]string, len(batch))
			for i, ch := range batch {
				texts[i] = ch.Text
			}

			vectors, err := idx.embedder.Embed(ctx, texts)
			if err != nil {
				stats.Failed += len(batch)
				idx.logger.Error("embed batch failed",
					"workspace_chat_id", chat.WorkspaceChatID,
					"batch_size", len(batch),
					"error", err,
				)
				break
			}

			for i, ch := range batch {
				if err := idx.store.UpsertMessageChunk(ctx, storage.MessageChunk{
					WorkspaceChatID: ch.WorkspaceChatID,
					MessageThreadID: ch.MessageThreadID,
					FirstMessageID:  ch.FirstMessageID,
					LastMessageID:   ch.LastMessageID,
					MinCreatedAt:    ch.MinCreatedAt,
					MaxCreatedAt:    ch.MaxCreatedAt,
					ChunkText:       ch.Text,
					ContentHash:     ch.ContentHash,
					Model:           model,
					Embedding:       vectors[i],
				}); err != nil {
					stats.Failed++
					idx.logger.Error("persist message chunk failed", "error", err)
					continue
				}
				stats.Indexed++
			}
		}
	}

	return stats, nil
}
