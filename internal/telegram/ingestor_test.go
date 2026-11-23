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
		chatID   int64
		text     string
		keyboard *InlineKeyboardMarkup
	}
	editedMessages []struct {
		chatID    int64
		messageID int64
		text      string
	}
	answeredCallbacks []string
	commandsSet       bool
	fileData          map[string][]byte
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
	return f.SendMessageWithKeyboard(context.Background(), chatID, text, nil)
}
func (f *fakeAPI) SendMessageWithKeyboard(_ context.Context, chatID int64, text string, kb *InlineKeyboardMarkup) error {
	f.sentMessages = append(f.sentMessages, struct {
		chatID   int64
		text     string
		keyboard *InlineKeyboardMarkup
	}{chatID: chatID, text: text, keyboard: kb})
	return nil
}
func (f *fakeAPI) EditMessageText(_ context.Context, chatID, messageID int64, text string) error {
	f.editedMessages = append(f.editedMessages, struct {
		chatID    int64
		messageID int64
		text      string
	}{chatID: chatID, messageID: messageID, text: text})
	return nil
}
func (f *fakeAPI) AnswerCallbackQuery(_ context.Context, callbackQueryID, _ string) error {
	f.answeredCallbacks = append(f.answeredCallbacks, callbackQueryID)
	return nil
}
func (f *fakeAPI) GetFile(_ context.Context, fileID string) (File, error) {
	return File{FileID: fileID, FilePath: "documents/" + fileID + ".json"}, nil
}
func (f *fakeAPI) DownloadFile(_ context.Context, filePath string, _ int64) ([]byte, error) {
	if f.fileData != nil {
		if data, ok := f.fileData[filePath]; ok {
			return data, nil
		}
	}
	return nil, nil
}

type fakeAccess struct {
	allowed map[int64]bool
}

func (f fakeAccess) HasAnyTrackedChatAccess(_ context.Context, userID int64) (bool, error) {
	return f.allowed[userID], nil
}

type fakeOAuthApprover struct {
	approved map[string]int64
	denied   map[string]bool
}

func (f *fakeOAuthApprover) ApproveLogin(_ context.Context, nonce string, userID int64) error {
	if f.approved == nil {
		f.approved = make(map[string]int64)
	}
	f.approved[nonce] = userID
	return nil
}

func (f *fakeOAuthApprover) DenyLogin(_ context.Context, nonce string) error {
	if f.denied == nil {
		f.denied = make(map[string]bool)
	}
	f.denied[nonce] = true
	return nil
}

func testConfig(t *testing.T, extra ...map[string]string) config.Config {
	t.Helper()
	m := map[string]string{
		"BOT_TOKEN":              "123:token",
		"ADMIN_TG_IDS":           "1000",
		"DEFAULT_HISTORY_POLICY": "full_history",
		"IGNORE_TAG":             "#offrecord",
	}
	for _, ov := range extra {
		for k, v := range ov {
			m[k] = v
		}
	}
	cfg, err := config.LoadFromLookup(func(k string) (string, bool) {
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

	threadID := int64(42)
	if err := ing.HandleUpdate(ctx, Update{
		UpdateID: 3,
		Message: &Message{
			MessageID:         10,
			MessageThreadID:   &threadID,
			Chat:              Chat{ID: -1001, Type: "group", Title: "Core"},
			Date:              1200,
			ForumTopicCreated: &ForumTopicCreated{Name: "Releases"},
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

func TestIngestorPrivateCommandsAndHideToken(t *testing.T) {
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
	if len(api.sentMessages) != 1 || !strings.Contains(api.sentMessages[0].text, "Access denied") {
		t.Fatalf("expected Access denied for outsider, got %+v", api.sentMessages)
	}

	// 2. Corporate member (user 200) receives a Bearer token + Hide token button via /auth.
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
	if len(api.sentMessages) != 2 || !strings.Contains(api.sentMessages[1].text, "tgmcp_") || api.sentMessages[1].keyboard == nil {
		t.Fatalf("expected token with inline keyboard for member, got %+v", api.sentMessages)
	}

	var token string
	for _, field := range strings.Fields(api.sentMessages[1].text) {
		if strings.HasPrefix(field, "tgmcp_") {
			token = strings.TrimRight(field, `").}`)
			break
		}
	}
	if uid, err := store.ValidateAuthToken(ctx, token, time.Now()); err != nil || uid != 200 {
		t.Fatalf("expected issued token to validate for 200, got (%d, %v)", uid, err)
	}

	// 2b. Clicking "Hide token" edits the message to remove the secret.
	if err := ing.HandleUpdate(ctx, Update{
		UpdateID: 20,
		CallbackQuery: &CallbackQuery{
			ID:      "cb_1",
			From:    User{ID: 200},
			Message: &Message{MessageID: 99, Chat: Chat{ID: 200, Type: "private"}},
			Data:    callbackHideToken,
		},
	}); err != nil {
		t.Fatalf("hide_token callback: %v", err)
	}
	if len(api.editedMessages) != 1 || !strings.Contains(api.editedMessages[0].text, "Token hidden") {
		t.Fatalf("expected message to be edited on hide_token, got %+v", api.editedMessages)
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
	if !strings.Contains(api.sentMessages[2].text, "restricted to administrators") {
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

func TestIngestorOAuthOnboardingAndDeepLinkApproval(t *testing.T) {
	ctx := context.Background()
	store := openIngestorStore(t)
	api := &fakeAPI{me: User{ID: 555, Username: "corp_bot", FirstName: "Corp", CanReadAllGroupMessages: true}}
	cfg := testConfig(t, map[string]string{
		"OAUTH_ENABLED":   "true",
		"PUBLIC_BASE_URL": "https://tg-mcp.company.internal",
	})
	approver := &fakeOAuthApprover{}
	ing := NewIngestor(api, store, cfg, fakeAccess{allowed: map[int64]bool{200: true}}, nil)
	ing.SetOAuthApprover(approver)
	_, _ = ing.Bootstrap(ctx)

	// 1. Outsider running /start must NOT see PUBLIC_BASE_URL or setup prompt.
	_ = ing.HandleUpdate(ctx, Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 1,
			Chat:      Chat{ID: 999, Type: "private"},
			From:      &User{ID: 999},
			Text:      "/start",
		},
	})
	if strings.Contains(api.sentMessages[0].text, "tg-mcp.company.internal") {
		t.Fatalf("outsider must not receive internal server URL: %q", api.sentMessages[0].text)
	}

	// 2. Workspace member running /start receives OAuth 2.1 AI agent prompt with bot username and no admin commands.
	_ = ing.HandleUpdate(ctx, Update{
		UpdateID: 2,
		Message: &Message{
			MessageID: 2,
			Chat:      Chat{ID: 200, Type: "private"},
			From:      &User{ID: 200},
			Text:      "/start",
		},
	})
	if !strings.Contains(api.sentMessages[1].text, "https://tg-mcp.company.internal/mcp") ||
		!strings.Contains(api.sentMessages[1].text, `"corp_bot"`) ||
		strings.Contains(api.sentMessages[1].text, "Admin commands") {
		t.Fatalf("expected OAuth onboarding prompt with bot username and no admin commands, got %q", api.sentMessages[1].text)
	}

	// 2b. Even admin running /start does NOT get admin commands in the connection prompt; /admin shows them.
	_ = ing.HandleUpdate(ctx, Update{
		UpdateID: 20,
		Message: &Message{
			MessageID: 20,
			Chat:      Chat{ID: 1000, Type: "private"},
			From:      &User{ID: 1000},
			Text:      "/start",
		},
	})
	if strings.Contains(api.sentMessages[len(api.sentMessages)-1].text, "Admin commands") {
		t.Fatalf("connection prompt must not include admin commands, got %q", api.sentMessages[len(api.sentMessages)-1].text)
	}
	_ = ing.HandleUpdate(ctx, Update{
		UpdateID: 21,
		Message: &Message{
			MessageID: 21,
			Chat:      Chat{ID: 1000, Type: "private"},
			From:      &User{ID: 1000},
			Text:      "/admin",
		},
	})
	if !strings.Contains(api.sentMessages[len(api.sentMessages)-1].text, "/status") {
		t.Fatalf("expected /admin to list admin commands for admin, got %q", api.sentMessages[len(api.sentMessages)-1].text)
	}

	// 3. Member approves OAuth deep-link /start login_nonce123.
	_ = ing.HandleUpdate(ctx, Update{
		UpdateID: 3,
		Message: &Message{
			MessageID: 3,
			Chat:      Chat{ID: 200, Type: "private"},
			From:      &User{ID: 200},
			Text:      "/start login_nonce123",
		},
	})
	if approver.approved["nonce123"] != 200 {
		t.Fatalf("expected nonce123 approved for user 200, got %+v", approver.approved)
	}

	// 4. Outsider clicking OAuth deep-link /start login_nonce456 denies session.
	_ = ing.HandleUpdate(ctx, Update{
		UpdateID: 4,
		Message: &Message{
			MessageID: 4,
			Chat:      Chat{ID: 999, Type: "private"},
			From:      &User{ID: 999},
			Text:      "/start login_nonce456",
		},
	})
	if !approver.denied["nonce456"] {
		t.Fatalf("expected nonce456 denied for outsider, got %+v", approver.denied)
	}
}

func TestIngestorDowntimeAlertAndPrivateJSONImport(t *testing.T) {
	ctx := context.Background()
	store := openIngestorStore(t)

	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	oldPoll := now.Add(-26 * time.Hour).Unix()
	_ = store.UpsertBot(ctx, storage.Bot{BotID: 555, Username: "corp_bot"})
	_ = store.UpdateBotOffset(ctx, 555, 10, oldPoll)

	sampleJSON := []byte(`{
		"name": "Recovered Chat",
		"type": "private_supergroup",
		"id": 999888777,
		"messages": [
			{"id": 1, "type": "message", "date_unixtime": "1795250000", "from": "Alice", "from_id": "user101", "text": "Recovered weekend discussion"},
			{"id": 2, "type": "message", "date_unixtime": "1795250100", "from": "Bob", "from_id": "user102", "text": "Secret #offrecord"}
		]
	}`)

	api := &fakeAPI{
		me:       User{ID: 555, Username: "corp_bot", FirstName: "Corp", CanReadAllGroupMessages: true},
		fileData: map[string][]byte{"documents/file_json_1.json": sampleJSON},
	}

	ing := NewIngestor(api, store, testConfig(t), fakeAccess{}, nil)
	ing.nowFunc = func() time.Time { return now }

	if _, err := ing.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if len(api.sentMessages) != 1 || api.sentMessages[0].chatID != 1000 || !strings.Contains(api.sentMessages[0].text, "Extended server downtime") {
		t.Fatalf("expected downtime alert to admin 1000, got %+v", api.sentMessages)
	}

	if err := ing.HandleUpdate(ctx, Update{
		UpdateID: 10,
		Message: &Message{
			MessageID: 10,
			Chat:      Chat{ID: 200, Type: "private"},
			From:      &User{ID: 200},
			Document:  &Document{FileID: "file_json_1", FileName: "result.json", MimeType: "application/json", FileSize: 200},
		},
	}); err != nil {
		t.Fatalf("non-admin doc import: %v", err)
	}
	if !strings.Contains(api.sentMessages[1].text, "restricted to administrators") {
		t.Fatalf("expected admin-only rejection, got %q", api.sentMessages[1].text)
	}

	if err := ing.HandleUpdate(ctx, Update{
		UpdateID: 11,
		Message: &Message{
			MessageID: 11,
			Chat:      Chat{ID: 1000, Type: "private"},
			From:      &User{ID: 1000},
			Document:  &Document{FileID: "file_json_1", FileName: "result.json", MimeType: "application/json", FileSize: 200},
		},
	}); err != nil {
		t.Fatalf("admin doc import: %v", err)
	}
	if !strings.Contains(api.sentMessages[2].text, "Imported messages: 1") {
		t.Fatalf("expected import summary with 1 imported message, got %q", api.sentMessages[2].text)
	}

	wsID, err := store.ResolveWorkspaceChatID(ctx, -100999888777)
	if err != nil {
		t.Fatalf("resolve imported chat: %v", err)
	}
	_ = store.UpsertChatACL(ctx, storage.ChatACL{
		WorkspaceChatID: wsID,
		TelegramUserID:  1000,
		Status:          "administrator",
		IsMember:        true,
		CheckedAt:       now.Unix(),
	})
	found, err := store.SearchMessages(ctx, storage.SearchParams{
		TelegramUserID:       1000,
		Query:                "Recovered",
		DefaultHistoryPolicy: config.HistoryPolicyFull,
	})
	if err != nil || len(found) != 1 {
		t.Fatalf("expected recovered message in FTS5, got (%+v, %v)", found, err)
	}

	// Admin sends Telegram for macOS HTML export (messages.html) -> auto-matched to existing chat "Recovered Chat" by title.
	htmlPayload := []byte(`<!DOCTYPE html><html><head><title>Recovered Chat</title></head><body>` +
		`<div class="page_header"><div class="name bold">Recovered Chat</div></div>` +
		`<div class="message default clearfix" id="message99"><div class="body">` +
		`<div class="pull_right date details" title="25 May 2026, 12:00:00">12:00</div>` +
		`<div class="from_name">Bob</div><div class="text">HTML imported note</div></div></div></body></html>`)
	api.fileData["documents/file_html_1.json"] = htmlPayload

	if err := ing.HandleUpdate(ctx, Update{
		UpdateID: 12,
		Message: &Message{
			MessageID: 12,
			Chat:      Chat{ID: 1000, Type: "private"},
			From:      &User{ID: 1000},
			Document:  &Document{FileID: "file_html_1", FileName: "messages.html", MimeType: "text/html", FileSize: int64(len(htmlPayload))},
		},
	}); err != nil {
		t.Fatalf("admin html doc import: %v", err)
	}
	if !strings.Contains(api.sentMessages[3].text, "Imported messages: 1") {
		t.Fatalf("expected HTML import summary with 1 imported message, got %q", api.sentMessages[3].text)
	}
	foundHTML, err := store.SearchMessages(ctx, storage.SearchParams{
		TelegramUserID:       1000,
		Query:                "HTML imported",
		DefaultHistoryPolicy: config.HistoryPolicyFull,
	})
	if err != nil || len(foundHTML) != 1 {
		t.Fatalf("expected HTML imported message in FTS5, got (%+v, %v)", foundHTML, err)
	}
}
