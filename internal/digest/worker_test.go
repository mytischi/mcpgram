package digest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mytischi/mcpgram/internal/config"
	"github.com/mytischi/mcpgram/internal/storage"
)

func TestOpenAIClientSummarizeAndMapReduce(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// Return markdown-fenced JSON on call 1 to verify resilient parsing.
		content := "```json\n{\"summary\":\"Обсудили релиз v0.2\",\"key_decisions\":[\"Выкатываем в пятницу\"],\"action_items\":[\"Matvei: подготовить тег\"],\"topics\":[\"релиз\"]}\n```"
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "qwen2.5:7b",
			"choices": []map[string]any{
				{"message": map[string]string{"content": content}},
			},
		})
	}))
	defer srv.Close()

	client := NewOpenAIClient(srv.URL+"/v1", "test-key", "qwen2.5:7b")
	client.maxChunkChars = 40 // force multi-chunk map-reduce

	transcript := "[10:00 #1] Alice: Первое длинное сообщение про релиз v0.2\n[10:05 #2] Bob: Второе длинное сообщение про выкатку в пятницу"
	res, err := client.SummarizeDay(context.Background(), "Core", "2026-11-21", transcript)
	if err != nil {
		t.Fatalf("SummarizeDay failed: %v", err)
	}
	if calls != 3 { // 2 chunks + 1 merge call
		t.Fatalf("expected 3 LLM calls for 2 chunks + merge, got %d", calls)
	}
	if res.Summary != "Обсудили релиз v0.2" || len(res.KeyDecisions) != 1 || len(res.ActionItems) != 1 {
		t.Fatalf("unexpected summary result: %+v", res)
	}
}

func TestOpenAIClientRedactsAPIKeyOnError(t *testing.T) {
	client := NewOpenAIClient("http://127.0.0.1:1/v1", "SECRET_LLM_KEY_123", "model")
	_, err := client.SummarizeDay(context.Background(), "Chat", "2026-11-21", "hello")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "SECRET_LLM_KEY_123") {
		t.Fatalf("API key leaked in error: %v", err)
	}
}

type fakeSummarizer struct {
	calls int
}

func (f *fakeSummarizer) SummarizeDay(_ context.Context, chatTitle, digestDate, _ string) (SummaryResult, error) {
	f.calls++
	return SummaryResult{
		Summary:      "Digest for " + chatTitle + " on " + digestDate,
		KeyDecisions: []string{"Decision A"},
		ActionItems:  []string{"Alice: follow up"},
		Topics:       []string{"ops"},
		Model:        "test-model",
	}, nil
}

func TestWorkerGeneratePendingIdempotency(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	chat, _ := store.UpsertChat(ctx, storage.Chat{TelegramChatID: -1001, Title: "Backend", ChatType: "supergroup"})
	quietChat, _ := store.UpsertChat(ctx, storage.Chat{TelegramChatID: -1002, Title: "Quiet", ChatType: "supergroup"})

	// Yesterday = 2026-11-21 in UTC. Put 3 messages in Backend, only 1 in Quiet.
	dayBase := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC).Unix()
	for i := int64(1); i <= 3; i++ {
		_, _ = store.UpsertMessage(ctx, storage.Message{
			WorkspaceChatID:   chat.WorkspaceChatID,
			TelegramMessageID: i,
			SenderName:        "Alice",
			Text:              "Important discussion line",
			CreatedAt:         dayBase + i*60,
		}, "")
	}
	_, _ = store.UpsertMessage(ctx, storage.Message{
		WorkspaceChatID:   quietChat.WorkspaceChatID,
		TelegramMessageID: 1,
		SenderName:        "Bob",
		Text:              "Ok",
		CreatedAt:         dayBase + 60,
	}, "")

	cfg, err := config.LoadFromLookup(func(k string) (string, bool) {
		m := map[string]string{
			"DIGEST_MODE":          "openai_compatible",
			"DIGEST_BASE_URL":      "http://127.0.0.1:11434/v1",
			"DIGEST_MODEL":         "test-model",
			"DIGEST_MIN_MESSAGES":  "3",
			"DIGEST_LOOKBACK_DAYS": "2",
			"DIGEST_TIMEZONE":      "UTC",
		}
		v, ok := m[k]
		return v, ok
	})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	sum := &fakeSummarizer{}
	worker, err := NewWorker(store, sum, cfg, nil)
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	worker.nowFunc = func() time.Time {
		return time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)
	}

	// First run: generates 1 digest (Backend 2026-11-21), skips Quiet + 2026-11-20.
	stats, err := worker.GeneratePending(ctx)
	if err != nil {
		t.Fatalf("generate pending #1: %v", err)
	}
	if stats.Generated != 1 || sum.calls != 1 {
		t.Fatalf("expected 1 generated digest, got stats=%+v calls=%d", stats, sum.calls)
	}

	// Second run with no new messages: 100% idempotent, 0 LLM calls.
	stats, err = worker.GeneratePending(ctx)
	if err != nil {
		t.Fatalf("generate pending #2: %v", err)
	}
	if stats.Generated != 0 || sum.calls != 1 {
		t.Fatalf("expected 0 regenerated digests, got stats=%+v calls=%d", stats, sum.calls)
	}

	// Add a 4th message (e.g. via desktop import) -> regenerates cleanly.
	_, _ = store.UpsertMessage(ctx, storage.Message{
		WorkspaceChatID:   chat.WorkspaceChatID,
		TelegramMessageID: 4,
		SenderName:        "Bob",
		Text:              "Late evening decision",
		CreatedAt:         dayBase + 3600,
	}, "")
	stats, err = worker.GeneratePending(ctx)
	if err != nil {
		t.Fatalf("generate pending #3: %v", err)
	}
	if stats.Generated != 1 || sum.calls != 2 {
		t.Fatalf("expected regeneration after new message, got stats=%+v calls=%d", stats, sum.calls)
	}
}

func TestNextScheduleTime(t *testing.T) {
	loc, _ := time.LoadLocation("UTC")
	// At 02:30 -> runs today at 03:00.
	n1 := time.Date(2026, 11, 22, 2, 30, 0, 0, loc)
	got1 := nextScheduleTime(n1, 3, loc)
	want1 := time.Date(2026, 11, 22, 3, 0, 0, 0, loc)
	if !got1.Equal(want1) {
		t.Fatalf("got %v, want %v", got1, want1)
	}

	// At 03:00 -> runs tomorrow at 03:00.
	n2 := time.Date(2026, 11, 22, 3, 0, 0, 0, loc)
	got2 := nextScheduleTime(n2, 3, loc)
	want2 := time.Date(2026, 11, 23, 3, 0, 0, 0, loc)
	if !got2.Equal(want2) {
		t.Fatalf("got %v, want %v", got2, want2)
	}
}
