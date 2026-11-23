package importer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mytischi/mcpgram/internal/config"
	"github.com/mytischi/mcpgram/internal/storage"
)

func TestImportHTMLMacOSAutoMatchesTrackedChat(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// Pre-register tracked workspace chat (as when bot was added to the group).
	chat, err := store.UpsertChat(ctx, storage.Chat{
		TelegramChatID: -1003377500943,
		Title:          "NWDS_Бронная_work",
		ChatType:       "supergroup",
	})
	if err != nil {
		t.Fatalf("upsert chat: %v", err)
	}

	sampleHTML := `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><title>NWDS_Бронная_work</title></head>
<body>
<div class="page_header chat_header">
  <div class="body"><div class="name bold">NWDS_Бронная_work</div></div>
</div>
<div class="history">
  <div class="message service"><div class="body details">25 May 2026</div></div>
  <div class="message default clearfix" id="message455104">
    <div class="pull_left userpic_wrap"><img class="userpic" src="photos/author_252740745.jpg"/></div>
    <div class="body">
      <div class="pull_right date details" title="25 May 2026, 19:42:02">19:42</div>
      <div class="from_name">Daria Karmazina</div>
      <div class="text">First line<br>Second line &amp; details</div>
    </div>
  </div>
  <div class="message default clearfix joined" id="message455105">
    <div class="body">
      <div class="pull_right date details" title="25 May 2026, 19:42:25">19:42</div>
      <div class="reply_to details">In reply to <a href="#go_to_message455104" onclick="return GoToMessage(455104)">this message</a></div>
      <div class="forwarded_from details">Forwarded from Artem</div>
      <div class="media_wrap clearfix">
        <div class="media clearfix pull_left media_file">
          <div class="body"><div class="title bold">Spec.pdf</div></div>
        </div>
      </div>
    </div>
  </div>
  <div class="message default clearfix" id="message455106">
    <div class="pull_left userpic_wrap"><img class="userpic" src="photos/author_323291081.jpg"/></div>
    <div class="body">
      <div class="pull_right date details" title="25 May 2026, 19:43:00">19:43</div>
      <div class="from_name">Matvei Naumov</div>
      <div class="text">Secret note #offrecord</div>
    </div>
  </div>
</div>
</body>
</html>`

	// Auto-detect HTML via ImportDesktopReader with NO chat_id override!
	stats, err := ImportDesktopReader(ctx, store, strings.NewReader(sampleHTML), Options{
		IgnoreTag: "#offrecord",
		Timezone:  "Europe/Moscow",
	})
	if err != nil {
		t.Fatalf("import macOS HTML: %v", err)
	}
	if stats.WorkspaceChatID != chat.WorkspaceChatID || stats.TelegramChatID != -1003377500943 {
		t.Fatalf("expected auto-matched chat %s (-1003377500943), got %+v", chat.WorkspaceChatID, stats)
	}
	if stats.Imported != 2 || stats.IgnoredOffrec != 1 || stats.SkippedNonMsg != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	_ = store.UpsertChatACL(ctx, storage.ChatACL{
		WorkspaceChatID: chat.WorkspaceChatID,
		TelegramUserID:  323291081,
		Status:          "member",
		IsMember:        true,
		CheckedAt:       time.Now().Unix(),
	})

	msgs, err := store.GetMessages(ctx, storage.MessageFilter{
		TelegramUserID:       323291081,
		WorkspaceChatID:      chat.WorkspaceChatID,
		DefaultHistoryPolicy: config.HistoryPolicyFull,
	})
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Text != "First line\nSecond line & details" {
		t.Fatalf("unexpected msg[0] text: %q", msgs[0].Text)
	}
	if msgs[0].SenderTGID == nil || *msgs[0].SenderTGID != 252740745 {
		t.Fatalf("expected author ID 252740745, got %v", msgs[0].SenderTGID)
	}
	// Joined message inherits author & ID, keeps reply_to and attachment metadata.
	if msgs[1].SenderName != "Daria Karmazina" || msgs[1].SenderTGID == nil || *msgs[1].SenderTGID != 252740745 {
		t.Fatalf("expected joined message to inherit sender, got name=%q id=%v", msgs[1].SenderName, msgs[1].SenderTGID)
	}
	if msgs[1].ReplyToMessageID == nil || *msgs[1].ReplyToMessageID != 455104 {
		t.Fatalf("expected reply_to 455104, got %v", msgs[1].ReplyToMessageID)
	}
	if msgs[1].AttachmentMeta == nil || msgs[1].AttachmentMeta.FileName != "Spec.pdf" {
		t.Fatalf("expected attachment Spec.pdf, got %+v", msgs[1].AttachmentMeta)
	}
	if !strings.Contains(msgs[1].Text, "[Forwarded from Artem]") {
		t.Fatalf("expected forwarded label, got %q", msgs[1].Text)
	}
}

func TestImportHTMLDesktopFormatAndExplicitChatID(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	desktopHTML := `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><title>Exported Data</title></head>
<body>
<div class="page_header"><div class="content"><div class="text bold">Backend Team</div></div></div>
<div class="history">
  <div class="message default clearfix" id="message10">
    <div class="body">
      <div class="pull_right date details" title="25.05.2026 19:42:02 UTC+03:00">19:42</div>
      <div class="from_name">Alice</div>
      <div class="text">Deployed <a href="https://example.com">v0.3</a></div>
    </div>
  </div>
</div>
</body>
</html>`

	// Without tracked chat and without override -> clear actionable error.
	if _, err := ImportDesktopReader(ctx, store, strings.NewReader(desktopHTML), Options{}); err == nil {
		t.Fatal("expected error when HTML chat title is not tracked and no --chat-id override provided")
	}

	// With explicit override -> succeeds and preserves UTC+03:00 timestamp accurately.
	stats, err := ImportDesktopReader(ctx, store, strings.NewReader(desktopHTML), Options{
		TelegramChatIDOverride: -100999888777,
	})
	if err != nil {
		t.Fatalf("import desktop HTML with override: %v", err)
	}
	if stats.ChatTitle != "Backend Team" || stats.Imported != 1 || stats.TelegramChatID != -100999888777 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	_ = store.UpsertChatACL(ctx, storage.ChatACL{
		WorkspaceChatID: stats.WorkspaceChatID,
		TelegramUserID:  1,
		Status:          "member",
		IsMember:        true,
		CheckedAt:       time.Now().Unix(),
	})
	msgs, _ := store.GetMessages(ctx, storage.MessageFilter{
		TelegramUserID:       1,
		WorkspaceChatID:      stats.WorkspaceChatID,
		DefaultHistoryPolicy: config.HistoryPolicyFull,
	})
	wantUnix := time.Date(2026, 5, 25, 16, 42, 2, 0, time.UTC).Unix()
	if len(msgs) != 1 || msgs[0].CreatedAt != wantUnix || msgs[0].Text != "Deployed v0.3" {
		t.Fatalf("unexpected desktop HTML message: %+v (want unix %d)", msgs, wantUnix)
	}
}
