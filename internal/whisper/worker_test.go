package whisper

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
	"github.com/mytischi/mcpgram/internal/telegram"
)

type fakeFetcher struct {
	data []byte
	err  error
}

func (f *fakeFetcher) GetFile(_ context.Context, fileID string) (telegram.File, error) {
	if f.err != nil {
		return telegram.File{}, f.err
	}
	return telegram.File{FileID: fileID, FilePath: "voice/file_1.oga", FileSize: int64(len(f.data))}, nil
}

func (f *fakeFetcher) DownloadFile(_ context.Context, _ string, _ int64) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.data, nil
}

func TestOpenAIClientMultipartAndRedaction(t *testing.T) {
	var gotModel, gotLang, gotFilename string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer whisper-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotModel = r.FormValue("model")
		gotLang = r.FormValue("language")
		_, hdr, err := r.FormFile("file")
		if err == nil && hdr != nil {
			gotFilename = hdr.Filename
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"text": "Согласовали бюджет на инфраструктуру",
		})
	}))
	defer srv.Close()

	client := NewOpenAIClient(srv.URL+"/v1", "whisper-secret", "faster-whisper-small", "ru")
	text, err := client.Transcribe(context.Background(), "voice_1.oga", []byte("fake-ogg-bytes"))
	if err != nil {
		t.Fatalf("Transcribe failed: %v", err)
	}
	if text != "Согласовали бюджет на инфраструктуру" {
		t.Fatalf("unexpected transcript: %q", text)
	}
	if gotModel != "faster-whisper-small" || gotLang != "ru" || gotFilename != "voice_1.oga" {
		t.Fatalf("unexpected multipart params: model=%q lang=%q file=%q", gotModel, gotLang, gotFilename)
	}

	// API key must be redacted on error.
	badClient := NewOpenAIClient("http://127.0.0.1:1/v1", "SECRET_WHISPER_TOKEN", "whisper-1", "")
	_, err = badClient.Transcribe(context.Background(), "a.ogg", []byte("123"))
	if err == nil || strings.Contains(err.Error(), "SECRET_WHISPER_TOKEN") {
		t.Fatalf("expected redacted network error, got %v", err)
	}
}

type fakeTranscriber struct {
	text string
	err  error
}

func (f *fakeTranscriber) Transcribe(_ context.Context, _ string, _ []byte) (string, error) {
	return f.text, f.err
}

func TestWorkerEndToEndUpdatesMessageAndFTS5(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	chat, _ := store.UpsertChat(ctx, storage.Chat{TelegramChatID: -1001, Title: "VoiceOps"})
	_ = store.UpsertChatACL(ctx, storage.ChatACL{
		WorkspaceChatID: chat.WorkspaceChatID,
		TelegramUserID:  101,
		Status:          "member",
		IsMember:        true,
		CheckedAt:       5000,
	})

	// Insert a voice message with empty text.
	_, err = store.UpsertMessage(ctx, storage.Message{
		WorkspaceChatID:   chat.WorkspaceChatID,
		TelegramMessageID: 1,
		SenderName:        "Alice",
		Text:              "",
		AttachmentMeta:    &storage.AttachmentMeta{Type: "voice", FileID: "voice_file_1", MimeType: "audio/ogg"},
		CreatedAt:         1000,
	}, "")
	if err != nil {
		t.Fatalf("upsert voice message: %v", err)
	}

	cfg, _ := config.LoadFromLookup(func(k string) (string, bool) {
		m := map[string]string{
			"WHISPER_MODE":     "openai_compatible",
			"WHISPER_BASE_URL": "http://127.0.0.1:8000/v1",
			"WHISPER_MODEL":    "whisper-1",
		}
		v, ok := m[k]
		return v, ok
	})

	worker := NewWorker(
		store,
		&fakeFetcher{data: []byte("ogg-audio")},
		&fakeTranscriber{text: "Запускаем миграцию базы в четверг"},
		cfg,
		nil,
	)

	stats, err := worker.ProcessPending(ctx)
	if err != nil || stats.Completed != 1 || stats.Failed != 0 {
		t.Fatalf("ProcessPending failed: stats=%+v err=%v", stats, err)
	}

	// Transcript must now be immediately searchable via FTS5!
	found, err := store.SearchMessages(ctx, storage.SearchParams{
		TelegramUserID:       101,
		Query:                "миграцию",
		DefaultHistoryPolicy: config.HistoryPolicyFull,
		MinACLCheckedAt:      4900,
	})
	if err != nil || len(found) != 1 || !strings.Contains(found[0].Text, "[voice transcript]: Запускаем миграцию базы в четверг") {
		t.Fatalf("expected voice transcript in FTS5 search, got (%+v, %v)", found, err)
	}

	// Second run has nothing left to transcribe.
	stats, err = worker.ProcessPending(ctx)
	if err != nil || stats.Completed != 0 {
		t.Fatalf("expected 0 pending after completion, got %+v", stats)
	}
}

func TestWorkerRetryCapOnFailure(t *testing.T) {
	ctx := context.Background()
	store, _ := storage.Open(ctx, ":memory:")
	defer store.Close()

	chat, _ := store.UpsertChat(ctx, storage.Chat{TelegramChatID: -1002, Title: "ErrChat"})
	_, _ = store.UpsertMessage(ctx, storage.Message{
		WorkspaceChatID:   chat.WorkspaceChatID,
		TelegramMessageID: 1,
		AttachmentMeta:    &storage.AttachmentMeta{Type: "video_note", FileID: "broken_id"},
		CreatedAt:         1000,
	}, "")

	cfg, _ := config.LoadFromLookup(func(k string) (string, bool) {
		m := map[string]string{
			"WHISPER_MODE":     "openai_compatible",
			"WHISPER_BASE_URL": "http://127.0.0.1:8000/v1",
		}
		v, ok := m[k]
		return v, ok
	})

	worker := NewWorker(store, &fakeFetcher{err: errors.New("download failed")}, &fakeTranscriber{}, cfg, nil)

	// Fails 3 times, then stops retrying on the 4th pass.
	for i := 0; i < maxTranscriptionAttempts; i++ {
		st, _ := worker.ProcessPending(ctx)
		if st.Failed != 1 {
			t.Fatalf("attempt %d: expected 1 failure, got %+v", i+1, st)
		}
	}
	st, _ := worker.ProcessPending(ctx)
	if st.Failed != 0 || st.Completed != 0 {
		t.Fatalf("expected job to stop retrying after max attempts, got %+v", st)
	}
}
