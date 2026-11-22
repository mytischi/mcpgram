package telegram

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mytischi/mcpgram/internal/config"
	"github.com/mytischi/mcpgram/internal/storage"
)

type fakeAPI struct {
	me           User
	sentMessages []struct {
		chatID int64
		text   string
	}
	commandsSet bool
}

func (f *fakeAPI) GetMe(_ context.Context) (User, error) { return f.me, nil }
func (f *fakeAPI) SetMyCommands(_ context.Context, _ []BotCommand) error {
	f.commandsSet = true
	return nil
}
func (f *fakeAPI) GetUpdates(_ context.Context, _ int64, _ int, _ []string) ([]Update, error) {
	return nil, nil
}
func (f *fakeAPI) GetChatMember(_ context.Context, _, _ int64) (ChatMember, error) {
	return ChatMember{}, nil
}
func (f *fakeAPI) SendMessage(_ context.Context, chatID int64, text string) error {
	f.sentMessages = append(f.sentMessages, struct {
		chatID int64
		text   string
	}{chatID: chatID, text: text})
	return nil
}

type fakeAccess struct {
	allowed map[int64]bool
}

func (f fakeAccess) HasAnyTrackedChatAccess(_ context.Context, userID int64) (bool, error) {
	return f.allowed[userID], nil
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.LoadFromLookup(func(k string) (string, bool) {
		m := map[string]string{
			"BOT_TOKEN":              "123:token",
			"ADMIN_TG_IDS":           "1000",
			"DEFAULT_HISTORY_POLICY": "full_history",
			"IGNORE_TAG":             "#offrecord",
		}
		v, ok := m[k]
		return v, ok
	})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

func openIngestorStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestIngestorMessagesTopicsMigrationAndACL(t *testing.T) {
	ctx := context.Background()
	store := openIngestorStore(t)
	api := &fakeAPI{me: User{ID: 555, Username: "corp_bot", FirstName: "Corp", CanReadAllGroupMessages: true}}
	ing := NewIngestor(api, store, testConfig(t), fakeAccess{allowed: map[int64]bool{200: true}}, nil)

	if _, err := ing.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !api.commandsSet {
		t.Fatal("expected SetMyCommands to be called")
	}

	// 1. Bot promoted to admin in group -1001.
	if err := ing.HandleUpdate(ctx, Update{
		UpdateID: 1,
		MyChatMember: &ChatMemberUpdated{
			Chat:          Chat{ID: -1001, Type: "group", Title: "Core"},
			Date:          1000,
			NewChatMember: ChatMember{Status: "administrator", User: User{ID: 555}},
		},
	}); err != nil {
		t.Fatalf("my_chat_member: %v", err)
	}

	// 2. User 200 joins group at t=1100.
	if err := ing.HandleUpdate(ctx, Update{
		UpdateID: 2,
		ChatMember: &ChatMemberUpdated{
			Chat:          Chat{ID: -1001, Type: "group", Title: "Core"},
			Date:          1100,
			OldChatMember: ChatMember{Status: "left", User: User{ID: 200}},
			NewChatMember: ChatMember{Status: "member", User: User{ID: 200}},
		},
	}); err != nil {
		t.Fatalf("chat_member join: %v", err)
	}

	// 3. Message with caption + document metadata, plus #offrecord message.
	threadID := int64(42)
	if err := ing.HandleUpdate(ctx, Update{
		UpdateID: 3,
		Message: &Message{
			MessageID:       10,
			MessageThreadID: &threadID,
			Chat:            Chat{ID: -1001, Type: "group", Title: "Core"},
			Date:            1200,
			ForumTopicCreated: &ForumTopicCreated{
				Name: "Releases",
			},
		},
	}); err != nil {
		t.Fatalf("topic created: %v", err)
	}

	if err := ing.HandleUpdate(ctx, Update{
		UpdateID: 4,
		Message: &Message{
			MessageID:       11,
			MessageThreadID: &threadID,
			From:            &User{ID: 200, FirstName: "Alice", Username: "alice"},
			Chat:            Chat{ID: -1001, Type: "group", Title: "Core"},
			Date:            1300,
			Caption:         "Q4 release checklist",
			Document:        &Document{FileID: "doc_1", FileName: "checklist.pdf", MimeType: "application/pdf", FileSize: 1024},
		},
	}); err != nil {
		t.Fatalf("document message: %v", err)
	}

	if err := ing.HandleUpdate(ctx, Update{
		UpdateID: 5,
		Message: &Message{
			MessageID: 12,
			From:      &User{ID: 200, FirstName: "Alice"},
			Chat:      Chat{ID: -1001, Type: "group", Title: "Core"},
			Date:      1400,
			Text:      "Secret #offrecord note",
		},
	}); err != nil {
		t.Fatalf("offrecord message: %v", err)
	}

	// 4. Group migrates to supergroup -100999.
	if err := ing.HandleUpdate(ctx, Update{
		UpdateID: 6,
		Message: &Message{
			MessageID:       13,
			Chat:            Chat{ID: -1001, Type: "group", Title: "Core"},
			Date:            1500,
			MigrateToChatID: -100999,
		},
	}); err != nil {
		t.Fatalf("migrate message: %v", err)
	}

	wsID, err := store.ResolveWorkspaceChatID(ctx, -100999)
	if err != nil {
		t.Fatalf("resolve migrated chat: %v", err)
	}

	msgs, err := store.GetMessages(ctx, storage.MessageFilter{
		TelegramUserID:       200,
		WorkspaceChatID:      wsID,
		DefaultHistoryPolicy: config.HistoryPolicyFull,
	})
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Text != "Q4 release checklist" || msgs[0].AttachmentMeta == nil || msgs[0].AttachmentMeta.FileName != "checklist.pdf" {
		t.Fatalf("unexpected stored messages: %+v", msgs)
	}

	chats, err := store.ListAccessibleChats(ctx, 200, config.HistoryPolicyFull)
	if err != nil || len(chats) != 1 || len(chats[0].Topics) != 1 || chats[0].Topics[0].Title != "Releases" {
		t.Fatalf("unexpected chats/topics: %+v (err=%v)", chats, err)
	}
}

func TestIngestorPrivateCommands(t *testing.T) {
	ctx := context.Background()
	store := openIngestorStore(t)
	api := &fakeAPI{me: User{ID: 555, Username: "corp_bot", FirstName: "Corp", CanReadAllGroupMessages: true}}
	ing := NewIngestor(api, store, testConfig(t), fakeAccess{allowed: map[int64]bool{200: true}}, nil)
	_, _ = ing.Bootstrap(ctx)

	// 1. Outsider (user 999) gets rejected by /auth.
	if err := ing.HandleUpdate(ctx, Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 1,
			Chat:      Chat{ID: 999, Type: "private"},
			From:      &User{ID: 999, FirstName: "Stranger"},
			Text:      "/auth",
		},
	}); err != nil {
		t.Fatalf("outsider /auth: %v", err)
	}
	if len(api.sentMessages) != 1 || !strings.Contains(api.sentMessages[0].text, "Доступ отклонён") {
		t.Fatalf("expected rejection for outsider, got %+v", api.sentMessages)
	}

	// 2. Corporate member (user 200) receives a Bearer token via /auth.
	if err := ing.HandleUpdate(ctx, Update{
		UpdateID: 2,
		Message: &Message{
			MessageID: 2,
			Chat:      Chat{ID: 200, Type: "private"},
			From:      &User{ID: 200, FirstName: "Alice"},
			Text:      "/auth",
		},
	}); err != nil {
		t.Fatalf("member /auth: %v", err)
	}
	if len(api.sentMessages) != 2 || !strings.Contains(api.sentMessages[1].text, "tgmcp_") {
		t.Fatalf("expected token for member, got %+v", api.sentMessages)
	}

	// Extract issued token and confirm it validates.
	var token string
	for _, field := range strings.Fields(api.sentMessages[1].text) {
		if strings.HasPrefix(field, "tgmcp_") {
			token = strings.TrimRight(field, `").`)
			break
		}
	}
	if uid, err := store.ValidateAuthToken(ctx, token, time.Now()); err != nil || uid != 200 {
		t.Fatalf("expected issued token to validate for 200, got (%d, %v)", uid, err)
	}

	// 3. Non-admin (200) cannot run /revoke_user.
	if err := ing.HandleUpdate(ctx, Update{
		UpdateID: 3,
		Message: &Message{
			MessageID: 3,
			Chat:      Chat{ID: 200, Type: "private"},
			From:      &User{ID: 200},
			Text:      "/revoke_user 200",
		},
	}); err != nil {
		t.Fatalf("non-admin revoke: %v", err)
	}
	if !strings.Contains(api.sentMessages[2].text, "только администраторам") {
		t.Fatalf("expected admin-only error, got %q", api.sentMessages[2].text)
	}

	// 4. Admin (1000) revokes user 200 -> token immediately invalid.
	if err := ing.HandleUpdate(ctx, Update{
		UpdateID: 4,
		Message: &Message{
			MessageID: 4,
			Chat:      Chat{ID: 1000, Type: "private"},
			From:      &User{ID: 1000},
			Text:      "/revoke_user 200",
		},
	}); err != nil {
		t.Fatalf("admin revoke: %v", err)
	}
	if _, err := store.ValidateAuthToken(ctx, token, time.Now()); err == nil {
		t.Fatal("expected token to be invalidated after /revoke_user")
	}

	// 5. Admin /status works.
	if err := ing.HandleUpdate(ctx, Update{
		UpdateID: 5,
		Message: &Message{
			MessageID: 5,
			Chat:      Chat{ID: 1000, Type: "private"},
			From:      &User{ID: 1000},
			Text:      "/status",
		},
	}); err != nil {
		t.Fatalf("admin status: %v", err)
	}
	if !strings.Contains(api.sentMessages[4].text, "mcpgram status") {
		t.Fatalf("unexpected /status output: %q", api.sentMessages[4].text)
	}

	// 6. Admin /set_policy toggles per-chat override and resets to default.
	chat, err := store.UpsertChat(ctx, storage.Chat{TelegramChatID: -10055, Title: "External", ChatType: "supergroup"})
	if err != nil {
		t.Fatalf("upsert chat: %v", err)
	}
	if err := ing.HandleUpdate(ctx, Update{
		UpdateID: 6,
		Message: &Message{
			MessageID: 6,
			Chat:      Chat{ID: 1000, Type: "private"},
			From:      &User{ID: 1000},
			Text:      "/set_policy " + chat.WorkspaceChatID + " since_join",
		},
	}); err != nil {
		t.Fatalf("admin /set_policy since_join: %v", err)
	}
	updated, err := store.GetChatByWorkspaceID(ctx, chat.WorkspaceChatID)
	if err != nil || updated.HistoryPolicyOverride == nil || *updated.HistoryPolicyOverride != config.HistoryPolicySinceJoin {
		t.Fatalf("expected since_join override, got %+v (err=%v)", updated.HistoryPolicyOverride, err)
	}

	if err := ing.HandleUpdate(ctx, Update{
		UpdateID: 7,
		Message: &Message{
			MessageID: 7,
			Chat:      Chat{ID: 1000, Type: "private"},
			From:      &User{ID: 1000},
			Text:      "/set_policy " + chat.WorkspaceChatID + " default",
		},
	}); err != nil {
		t.Fatalf("admin /set_policy default: %v", err)
	}
	updated, err = store.GetChatByWorkspaceID(ctx, chat.WorkspaceChatID)
	if err != nil || updated.HistoryPolicyOverride != nil {
		t.Fatalf("expected nil override after reset, got %+v (err=%v)", updated.HistoryPolicyOverride, err)
	}

	// 7. Admin /digest when disabled returns helpful notice.
	if err := ing.HandleUpdate(ctx, Update{
		UpdateID: 8,
		Message: &Message{
			MessageID: 8,
			Chat:      Chat{ID: 1000, Type: "private"},
			From:      &User{ID: 1000},
			Text:      "/digest",
		},
	}); err != nil {
		t.Fatalf("admin /digest disabled: %v", err)
	}
	lastMsg := api.sentMessages[len(api.sentMessages)-1].text
	if !strings.Contains(lastMsg, "DIGEST_MODE=openai_compatible") {
		t.Fatalf("expected disabled digest hint, got %q", lastMsg)
	}
}
