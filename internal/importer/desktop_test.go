package importer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mytischi/mcpgram/internal/config"
	"github.com/mytischi/mcpgram/internal/storage"
)

func TestImportDesktopReader(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	sampleJSON := `{
		"name": "Engineering",
		"type": "private_supergroup",
		"id": 1234567890,
		"messages": [
			{
				"id": 1,
				"type": "service",
				"action": "topic_created",
				"title": "Architecture",
				"date_unixtime": "1700000000"
			},
			{
				"id": 2,
				"type": "message",
				"date_unixtime": "1700000100",
				"from": "Alice",
				"from_id": "user101",
				"text": ["Check ", {"type": "link", "text": "https://example.com/spec"}, " please"]
			},
			{
				"id": 3,
				"type": "message",
				"date_unixtime": "1700000200",
				"from": "Bob",
				"from_id": "user202",
				"reply_to_message_id": 2,
				"text": "Looks good, attached PDF",
				"file": "files/spec.pdf",
				"file_name": "spec.pdf",
				"mime_type": "application/pdf",
				"file_size": 2048
			},
			{
				"id": 4,
				"type": "message",
				"date_unixtime": "1700000300",
				"from": "Alice",
				"from_id": "user101",
				"text": "Do not store this #offrecord"
			},
			{
				"id": 5,
				"type": "message",
				"date_unixtime": "1600000000",
				"from": "Alice",
				"from_id": "user101",
				"text": "Very old message"
			}
		]
	}`

	stats, err := ImportDesktopReader(ctx, store, strings.NewReader(sampleJSON), Options{
		IgnoreTag:     "#offrecord",
		RetentionDays: 90,
		Now:           time.Unix(1700100000, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("import desktop json: %v", err)
	}

	if stats.TelegramChatID != -1001234567890 {
		t.Fatalf("expected Bot API supergroup id -1001234567890, got %d", stats.TelegramChatID)
	}
	if stats.Imported != 2 || stats.IgnoredOffrec != 1 || stats.SkippedOld != 1 || stats.SkippedNonMsg != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	_ = store.UpsertChatACL(ctx, storage.ChatACL{
		WorkspaceChatID: stats.WorkspaceChatID,
		TelegramUserID:  101,
		Status:          "member",
		IsMember:        true,
		CheckedAt:       1700100000,
	})

	msgs, err := store.GetMessages(ctx, storage.MessageFilter{
		TelegramUserID:       101,
		WorkspaceChatID:      stats.WorkspaceChatID,
		DefaultHistoryPolicy: config.HistoryPolicyFull,
	})
	if err != nil {
		t.Fatalf("get imported messages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 imported messages, got %d", len(msgs))
	}
	if msgs[0].Text != "Check https://example.com/spec please" {
		t.Fatalf("unexpected flattened text: %q", msgs[0].Text)
	}
	if msgs[1].AttachmentMeta == nil || msgs[1].AttachmentMeta.FileName != "spec.pdf" {
		t.Fatalf("expected attachment metadata, got %+v", msgs[1].AttachmentMeta)
	}
}
