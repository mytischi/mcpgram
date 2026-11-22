package semantic

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/mytischi/mcpgram/internal/storage"
)

const (
	rrfK              = 60.0
	minSemanticScore  = 0.25
	maxSemanticChunks = 15
)

// HybridSearcher implements storage.Searcher by combining SQLite FTS5 (BM25) with
// RBAC pre-filtered semantic vector search via Reciprocal Rank Fusion (RRF).
// If embedder is nil or temporarily unavailable, it degrades gracefully to FTS5.
type HybridSearcher struct {
	store    *storage.Store
	embedder Embedder
	logger   *slog.Logger
}

var _ storage.Searcher = (*HybridSearcher)(nil)

// NewHybridSearcher wraps store's FTS5 search with optional vector RRF fusion.
func NewHybridSearcher(store *storage.Store, embedder Embedder, logger *slog.Logger) *HybridSearcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &HybridSearcher{
		store:    store,
		embedder: embedder,
		logger:   logger,
	}
}

// SearchMessages executes FTS5 search and, when an embedder is configured, merges RBAC pre-filtered
// semantic chunk matches using Reciprocal Rank Fusion (RRF).
func (h *HybridSearcher) SearchMessages(ctx context.Context, params storage.SearchParams) ([]storage.Message, error) {
	limit := params.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	// 1. Always run FTS5 (BM25) — unbeatable for exact IDs, ticket numbers, and names.
	ftsParams := params
	ftsParams.Limit = limit
	ftsMsgs, err := h.store.SearchMessages(ctx, ftsParams)
	if err != nil {
		return nil, err
	}

	if h.embedder == nil {
		return ftsMsgs, nil
	}

	// 2. Embed query; degrade gracefully to FTS5 on transient embedder failure.
	vecs, err := h.embedder.Embed(ctx, []string{params.Query})
	if err != nil || len(vecs) == 0 {
		h.logger.Warn("semantic query embedding failed; falling back to FTS5", "error", err)
		return ftsMsgs, nil
	}
	queryVec := storage.NormalizeVector(vecs[0])

	// 3. RBAC pre-filtered candidate chunks directly from SQLite (unauthorized chats NEVER enter scoring).
	chunks, err := h.store.ListAccessibleChunksPreFiltered(ctx, params, h.embedder.Model())
	if err != nil {
		h.logger.Warn("semantic chunk pre-filter failed; falling back to FTS5", "error", err)
		return ftsMsgs, nil
	}
	if len(chunks) == 0 {
		return ftsMsgs, nil
	}

	type scoredChunk struct {
		chunk storage.MessageChunk
		score float32
	}
	scored := make([]scoredChunk, 0, len(chunks))
	for _, ch := range chunks {
		if len(ch.Embedding) != len(queryVec) {
			continue
		}
		sim := storage.DotProduct(queryVec, ch.Embedding)
		if sim >= minSemanticScore {
			scored = append(scored, scoredChunk{chunk: ch, score: sim})
		}
	}
	if len(scored) == 0 {
		return ftsMsgs, nil
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})
	if len(scored) > maxSemanticChunks {
		scored = scored[:maxSemanticChunks]
	}

	// 4. Expand top semantic chunks into RBAC-filtered messages and fuse with FTS5 via RRF.
	type msgKey struct {
		chatID string
		msgID  int64
	}
	type rankedMsg struct {
		msg   storage.Message
		score float64
	}
	merged := make(map[msgKey]*rankedMsg)

	addScore := func(m storage.Message, rank int) {
		k := msgKey{chatID: m.WorkspaceChatID, msgID: m.TelegramMessageID}
		r, ok := merged[k]
		if !ok {
			r = &rankedMsg{msg: m}
			merged[k] = r
		}
		r.score += 1.0 / (rrfK + float64(rank+1))
	}

	for rank, m := range ftsMsgs {
		addScore(m, rank)
	}

	vecRank := 0
	seenInVec := make(map[msgKey]struct{})
	for _, sc := range scored {
		msgs, err := h.store.GetMessagesByIDRange(ctx, params, sc.chunk.WorkspaceChatID, sc.chunk.FirstMessageID, sc.chunk.LastMessageID)
		if err != nil {
			return nil, fmt.Errorf("expand semantic chunk: %w", err)
		}
		for _, m := range msgs {
			k := msgKey{chatID: m.WorkspaceChatID, msgID: m.TelegramMessageID}
			if _, dup := seenInVec[k]; dup {
				continue
			}
			seenInVec[k] = struct{}{}
			addScore(m, vecRank)
			vecRank++
		}
	}

	results := make([]rankedMsg, 0, len(merged))
	for _, r := range merged {
		results = append(results, *r)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score > results[j].score
		}
		return results[i].msg.CreatedAt > results[j].msg.CreatedAt
	})

	if len(results) > limit {
		results = results[:limit]
	}

	out := make([]storage.Message, len(results))
	for i, r := range results {
		out[i] = r.msg
	}
	return out, nil
}
