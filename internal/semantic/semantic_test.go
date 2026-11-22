package semantic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mytischi/mcpgram/internal/config"
	"github.com/mytischi/mcpgram/internal/storage"
)

func int64Ptr(v int64) *int64 { return &v }

func TestBuildDialogChunks(t *testing.T) {
	t1 := int64(10)
	t2 := int64(20)

	msgs := []storage.Message{
		// Short noise alone in a window -> skipped
		{WorkspaceChatID: "c1", TelegramMessageID: 1, SenderName: "A", Text: "Ок", CreatedAt: 100},
		// Topic 10, two messages within 10 minutes -> merged into 1 chunk
		{WorkspaceChatID: "c1", TelegramMessageID: 2, MessageThreadID: &t1, SenderName: "Alice", Text: "Выкатку перенесли на вторник", CreatedAt: 1000},
		{WorkspaceChatID: "c1", TelegramMessageID: 3, MessageThreadID: &t1, SenderName: "Bob", Text: "Из-за бага в оплате Stripe", CreatedAt: 1120},
		// Topic 20 at same time -> separate chunk (topics never mix)
		{WorkspaceChatID: "c1", TelegramMessageID: 4, MessageThreadID: &t2, SenderName: "Eve", Text: "Дизайн новой главной страницы готов", CreatedAt: 1130},
		// Topic 10 much later (> 600s) -> new chunk
		{WorkspaceChatID: "c1", TelegramMessageID: 5, MessageThreadID: &t1, SenderName: "Alice", Text: "Хотфикс оплат залит на прод", CreatedAt: 5000},
	}

	chunks := BuildDialogChunks(msgs, 600, 8)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 dialog chunks (skipping noise), got %d: %+v", len(chunks), chunks)
	}
	if chunks[0].FirstMessageID != 2 || chunks[0].LastMessageID != 3 {
		t.Fatalf("expected first chunk [2..3], got [%d..%d]", chunks[0].FirstMessageID, chunks[0].LastMessageID)
	}
	if chunks[1].FirstMessageID != 5 {
		t.Fatalf("expected second chunk on topic 10 [5..5], got [%d..%d]", chunks[1].FirstMessageID, chunks[1].LastMessageID)
	}
	if chunks[2].MessageThreadID == nil || *chunks[2].MessageThreadID != 20 {
		t.Fatalf("expected third chunk on topic 20, got %+v", chunks[2])
	}
}

type deterministicEmbedder struct {
	fail  bool
	calls int
}

func (d *deterministicEmbedder) Model() string { return "test-embed-v1" }

func (d *deterministicEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	d.calls++
	if d.fail {
		return nil, errors.New("embedder unavailable")
	}
	out := make([][]float32, len(inputs))
	for i, text := range inputs {
		low := strings.ToLower(text)
		// 3D semantic space: [release/deploy, billing/payroll, design]
		var v [3]float32
		if strings.Contains(low, "выкатк") || strings.Contains(low, "релиз") || strings.Contains(low, "деплой") {
			v[0] = 1.0
		}
		if strings.Contains(low, "зарплат") || strings.Contains(low, "бюджет") || strings.Contains(low, "оплат") {
			v[1] = 1.0
		}
		if strings.Contains(low, "дизайн") || strings.Contains(low, "макет") {
			v[2] = 1.0
		}
		if v == [3]float32{} {
			v[2] = 0.1
		}
		out[i] = v[:]
	}
	return out, nil
}

func TestIndexerAndHybridSearchWithRBACPreFiltering(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	engChat, _ := store.UpsertChat(ctx, storage.Chat{TelegramChatID: -1001, Title: "Engineering"})
	finChat, _ := store.UpsertChat(ctx, storage.Chat{TelegramChatID: -1002, Title: "Finance"})

	// Engineering message uses synonym ("выкатку сдвинули") while query asks ("перенесли релиз").
	_, _ = store.UpsertMessage(ctx, storage.Message{
		WorkspaceChatID:   engChat.WorkspaceChatID,
		TelegramMessageID: 1,
		SenderName:        "Alice",
		Text:              "Выкатку сдвинули на вторник из-за бага",
		CreatedAt:         1000,
	}, "")
	// Finance chat (user 101 is NOT a member!) also talks about release/budget.
	_, _ = store.UpsertMessage(ctx, storage.Message{
		WorkspaceChatID:   finChat.WorkspaceChatID,
		TelegramMessageID: 1,
		SenderName:        "CFO",
		Text:              "Секретный релиз бюджета зарплат",
		CreatedAt:         1000,
	}, "")

	_ = store.UpsertChatACL(ctx, storage.ChatACL{
		WorkspaceChatID: engChat.WorkspaceChatID,
		TelegramUserID:  101,
		Status:          "member",
		IsMember:        true,
		JoinedAt:        int64Ptr(500),
		CheckedAt:       5000,
	})

	cfg, _ := config.LoadFromLookup(func(k string) (string, bool) {
		m := map[string]string{
			"EMBEDDING_MODE":     "openai_compatible",
			"EMBEDDING_BASE_URL": "http://127.0.0.1:11434/v1",
			"EMBEDDING_MODEL":    "test-embed-v1",
		}
		v, ok := m[k]
		return v, ok
	})

	emb := &deterministicEmbedder{}
	indexer := NewIndexer(store, emb, cfg, nil)

	stats, err := indexer.IndexAll(ctx)
	if err != nil {
		t.Fatalf("IndexAll #1: %v", err)
	}
	if stats.Indexed != 2 {
		t.Fatalf("expected 2 chunks indexed, got %+v", stats)
	}

	// Second IndexAll must be 100% idempotent (0 new embeddings).
	callsBefore := emb.calls
	stats, err = indexer.IndexAll(ctx)
	if err != nil {
		t.Fatalf("IndexAll #2: %v", err)
	}
	if stats.Indexed != 0 || stats.Skipped != 2 || emb.calls != callsBefore {
		t.Fatalf("expected idempotent skip, got stats=%+v calls=%d", stats, emb.calls)
	}

	// Hybrid search for "перенесли релиз":
	// - FTS5 finds nothing in Engineering (different words: "выкатку сдвинули")
	// - Semantic search finds Engineering chunk via vector similarity
	// - RBAC pre-filtering strictly excludes Finance chat!
	hybrid := NewHybridSearcher(store, emb, nil)
	res, err := hybrid.SearchMessages(ctx, storage.SearchParams{
		TelegramUserID:       101,
		Query:                "почему перенесли релиз?",
		DefaultHistoryPolicy: config.HistoryPolicyFull,
		MinACLCheckedAt:      4900,
	})
	if err != nil {
		t.Fatalf("hybrid search failed: %v", err)
	}
	if len(res) != 1 || res[0].WorkspaceChatID != engChat.WorkspaceChatID {
		t.Fatalf("expected only Engineering message via semantic match, got %+v", res)
	}

	// Graceful fallback to FTS5 when embedder is down.
	emb.fail = true
	ftsFallback, err := hybrid.SearchMessages(ctx, storage.SearchParams{
		TelegramUserID:       101,
		Query:                "вторник",
		DefaultHistoryPolicy: config.HistoryPolicyFull,
		MinACLCheckedAt:      4900,
	})
	if err != nil || len(ftsFallback) != 1 {
		t.Fatalf("expected graceful FTS5 fallback, got (%d, %v)", len(ftsFallback), err)
	}
}

func TestOpenAIEmbedderHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"index": 1, "embedding": []float32{0, 1}},
				{"index": 0, "embedding": []float32{1, 0}},
			},
		})
	}))
	defer srv.Close()

	client := NewOpenAIEmbedder(srv.URL+"/v1", "", "nomic-embed-text")
	vecs, err := client.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}
	if len(vecs) != 2 || vecs[0][0] != 1 || vecs[1][1] != 1 {
		t.Fatalf("expected index-ordered vectors, got %+v", vecs)
	}
}
