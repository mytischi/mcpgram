package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mytischi/mcpgram/internal/config"
	"github.com/mytischi/mcpgram/internal/rbac"
	"github.com/mytischi/mcpgram/internal/storage"
	"github.com/mytischi/mcpgram/internal/telegram"
)

type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (b bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if b.token != "" {
		clone.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.base.RoundTrip(clone)
}

type stubChecker struct {
	members map[int64]map[int64]string // chatID -> userID -> status
}

func (s *stubChecker) GetChatMember(_ context.Context, chatID, userID int64) (telegram.ChatMember, error) {
	if users, ok := s.members[chatID]; ok {
		if status, ok := users[userID]; ok {
			return telegram.ChatMember{Status: status}, nil
		}
	}
	return telegram.ChatMember{}, &telegram.APIError{
		Method:      "getChatMember",
		Code:        400,
		Description: "Bad Request: user not found",
	}
}

func TestStreamableHTTPMCPAuthAndRBACEndToEnd(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	chat1, _ := store.UpsertChat(ctx, storage.Chat{TelegramChatID: -1001, Title: "Backend", ChatType: "supergroup", BotIsAdmin: true, BotStatus: "administrator"})
	chat2, _ := store.UpsertChat(ctx, storage.Chat{TelegramChatID: -1002, Title: "Finance", ChatType: "supergroup", BotIsAdmin: true, BotStatus: "administrator"})

	replyTo := int64(1)
	_, _ = store.UpsertMessage(ctx, storage.Message{WorkspaceChatID: chat1.WorkspaceChatID, TelegramMessageID: 1, SenderName: "Alice", Text: "Deploy v1.2 on Friday", CreatedAt: 1700000000}, "")
	_, _ = store.UpsertMessage(ctx, storage.Message{WorkspaceChatID: chat1.WorkspaceChatID, TelegramMessageID: 2, SenderName: "Bob", Text: "Confirmed for Friday", ReplyToMessageID: &replyTo, CreatedAt: 1700000060}, "")
	_, _ = store.UpsertMessage(ctx, storage.Message{WorkspaceChatID: chat2.WorkspaceChatID, TelegramMessageID: 1, SenderName: "CFO", Text: "Payroll budget Friday", CreatedAt: 1700000000}, "")

	checker := &stubChecker{
		members: map[int64]map[int64]string{
			-1001: {101: "member"},
			-1002: {202: "member"},
		},
	}

	cfg, _ := config.LoadFromLookup(func(k string) (string, bool) {
		return "", false
	})
	authz := rbac.NewAuthorizer(store, checker, 5*time.Minute, nil)
	srv := New(store, store, authz, cfg, nil)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. /healthz works without auth.
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected /healthz 200, got (%v, %v)", resp, err)
	}
	_ = resp.Body.Close()

	// 2. /mcp rejects unauthenticated request with 401.
	resp, err = http.Post(ts.URL+"/mcp", "application/json", nil)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got (%v, %v)", resp, err)
	}
	_ = resp.Body.Close()

	// 3. Connect real MCP client as User 101 (member of Backend only).
	token101, _, err := store.IssueAuthToken(ctx, 101, 24*time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             ts.URL + "/mcp",
		DisableStandaloneSSE: true,
		HTTPClient: &http.Client{
			Transport: bearerTransport{base: http.DefaultTransport, token: token101},
		},
	}, nil)
	if err != nil {
		t.Fatalf("connect mcp client: %v", err)
	}
	defer session.Close()

	// 4. list_chats returns ONLY Backend, not Finance.
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_chats"})
	if err != nil || res.IsError {
		t.Fatalf("list_chats failed: res=%+v err=%v", res, err)
	}
	var chatsOut listChatsOutput
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &chatsOut); err != nil {
		t.Fatalf("decode list_chats output: %v", err)
	}
	if len(chatsOut.Chats) != 1 || chatsOut.Chats[0].Title != "Backend" {
		t.Fatalf("expected only Backend chat for user 101, got %+v", chatsOut.Chats)
	}

	// 5. search_messages finds Backend message and NEVER leaks Finance message.
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "search_messages",
		Arguments: map[string]any{"query": "Friday"},
	})
	if err != nil || res.IsError {
		t.Fatalf("search_messages failed: res=%+v err=%v", res, err)
	}
	var searchOut messagesOutput
	raw, _ = json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(raw, &searchOut)
	if searchOut.Count != 2 || searchOut.UntrustedContentNotice == "" {
		t.Fatalf("expected 2 Backend messages with untrusted notice, got %+v", searchOut)
	}
	for _, m := range searchOut.Messages {
		if m.ChatID != chat1.WorkspaceChatID {
			t.Fatalf("RBAC leak: got message from forbidden chat %s", m.ChatID)
		}
	}

	// 6. Explicit access to Finance chat is rejected as forbidden tool error.
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_messages",
		Arguments: map[string]any{"chat_id": chat2.WorkspaceChatID},
	})
	if err != nil {
		t.Fatalf("protocol error on forbidden chat: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected tool error (forbidden) for Finance chat, got %+v", res)
	}

	// 7. get_thread returns full reply chain [1, 2].
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_thread",
		Arguments: map[string]any{"chat_id": chat1.WorkspaceChatID, "message_id": 2},
	})
	if err != nil || res.IsError {
		t.Fatalf("get_thread failed: res=%+v err=%v", res, err)
	}
	var threadOut messagesOutput
	raw, _ = json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(raw, &threadOut)
	if threadOut.Count != 2 {
		t.Fatalf("expected 2 messages in thread, got %d", threadOut.Count)
	}

	// 8. Revoking user 101 immediately blocks further MCP requests with 401.
	if _, err := store.RevokeUserTokens(ctx, 101); err != nil {
		t.Fatalf("revoke tokens: %v", err)
	}
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_chats"}); err == nil {
		t.Fatal("expected HTTP 401 error after token revocation")
	}
}
