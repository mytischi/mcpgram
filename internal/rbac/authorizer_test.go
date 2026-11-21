package rbac

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mytischi/mcpgram/internal/config"
	"github.com/mytischi/mcpgram/internal/storage"
	"github.com/mytischi/mcpgram/internal/telegram"
)

type fakeChecker struct {
	calls int
	fn    func(chatID, userID int64) (telegram.ChatMember, error)
}

func (f *fakeChecker) GetChatMember(_ context.Context, chatID, userID int64) (telegram.ChatMember, error) {
	f.calls++
	return f.fn(chatID, userID)
}

func boolPtr(b bool) *bool { return &b }

func openTestStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAuthorizerHybridCacheAndFailClosed(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	chat, err := store.UpsertChat(ctx, storage.Chat{
		TelegramChatID: -10010,
		Title:          "Engineering",
		ChatType:       "supergroup",
		BotIsAdmin:     true,
		BotStatus:      "administrator",
	})
	if err != nil {
		t.Fatalf("upsert chat: %v", err)
	}

	now := time.Unix(10_000, 0).UTC()
	checker := &fakeChecker{
		fn: func(_, _ int64) (telegram.ChatMember, error) {
			return telegram.ChatMember{Status: "member"}, nil
		},
	}

	auth := NewAuthorizer(store, checker, 5*time.Minute, nil)
	auth.nowFunc = func() time.Time { return now }

	// 1. Cache miss triggers live getChatMember.
	allowed, err := auth.CheckChatAccess(ctx, chat.WorkspaceChatID, 101)
	if err != nil || !allowed {
		t.Fatalf("expected allowed=true on live check, got (%v, %v)", allowed, err)
	}
	if checker.calls != 1 {
		t.Fatalf("expected 1 live call, got %d", checker.calls)
	}

	// 2. Within TTL (2 minutes later), cache hit does not call Telegram API.
	now = now.Add(2 * time.Minute)
	allowed, err = auth.CheckChatAccess(ctx, chat.WorkspaceChatID, 101)
	if err != nil || !allowed {
		t.Fatalf("expected cached allowed=true, got (%v, %v)", allowed, err)
	}
	if checker.calls != 1 {
		t.Fatalf("expected cache hit (still 1 call), got %d", checker.calls)
	}

	// 3. Past TTL (6 minutes total) + network error -> strict fail-closed!
	now = now.Add(4 * time.Minute)
	checker.fn = func(_, _ int64) (telegram.ChatMember, error) {
		return telegram.ChatMember{}, errors.New("telegram network timeout")
	}
	allowed, err = auth.CheckChatAccess(ctx, chat.WorkspaceChatID, 101)
	if err == nil || allowed {
		t.Fatalf("expected fail-closed (false, err), got (%v, %v)", allowed, err)
	}

	// ListAccessibleChats must also exclude stale ACL entries when live refresh fails!
	summaries, err := auth.ListAccessibleChats(ctx, 101, config.HistoryPolicyFull)
	if err != nil {
		t.Fatalf("list accessible chats: %v", err)
	}
	if len(summaries) != 0 {
		t.Fatalf("expected 0 chats when stale ACL cannot be refreshed, got %d", len(summaries))
	}

	// 4. Restricted user with is_member=false must be denied.
	checker.fn = func(_, _ int64) (telegram.ChatMember, error) {
		return telegram.ChatMember{Status: "restricted", IsMember: boolPtr(false)}, nil
	}
	allowed, err = auth.CheckChatAccess(ctx, chat.WorkspaceChatID, 101)
	if err != nil || allowed {
		t.Fatalf("expected restricted is_member=false to be denied, got (%v, %v)", allowed, err)
	}

	// 5. Restricted user with is_member=true must be allowed.
	now = now.Add(6 * time.Minute)
	checker.fn = func(_, _ int64) (telegram.ChatMember, error) {
		return telegram.ChatMember{Status: "restricted", IsMember: boolPtr(true)}, nil
	}
	allowed, err = auth.CheckChatAccess(ctx, chat.WorkspaceChatID, 101)
	if err != nil || !allowed {
		t.Fatalf("expected restricted is_member=true to be allowed, got (%v, %v)", allowed, err)
	}
}

func TestAuthorizerHasAnyTrackedChatAccessForAuth(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	_, _ = store.UpsertChat(ctx, storage.Chat{
		TelegramChatID: -10020,
		Title:          "Corp",
		ChatType:       "supergroup",
		BotIsAdmin:     true,
		BotStatus:      "administrator",
	})

	checker := &fakeChecker{
		fn: func(_ int64, userID int64) (telegram.ChatMember, error) {
			if userID == 777 {
				return telegram.ChatMember{Status: "member"}, nil
			}
			return telegram.ChatMember{}, &telegram.APIError{
				Method:      "getChatMember",
				Code:        400,
				Description: "Bad Request: user not found",
			}
		},
	}

	auth := NewAuthorizer(store, checker, 5*time.Minute, nil)

	ok, err := auth.HasAnyTrackedChatAccess(ctx, 777)
	if err != nil || !ok {
		t.Fatalf("expected corporate member 777 to pass, got (%v, %v)", ok, err)
	}

	ok, err = auth.HasAnyTrackedChatAccess(ctx, 99999)
	if err != nil || ok {
		t.Fatalf("expected outsider 99999 to be rejected cleanly, got (%v, %v)", ok, err)
	}
}
