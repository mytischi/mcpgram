package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mytischi/mcpgram/internal/config"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func int64Ptr(v int64) *int64 { return &v }

func TestMigrationsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("second migrate failed: %v", err)
	}
}

func TestChatMigrationPreservesHistory(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	chat, err := s.UpsertChat(ctx, Chat{
		TelegramChatID: -1001,
		Title:          "Backend Team",
		ChatType:       "group",
		BotIsAdmin:     true,
		BotStatus:      "administrator",
	})
	if err != nil {
		t.Fatalf("upsert chat: %v", err)
	}

	if _, err := s.UpsertMessage(ctx, Message{
		WorkspaceChatID:   chat.WorkspaceChatID,
		TelegramMessageID: 10,
		SenderName:        "Alice",
		Text:              "Release plan before supergroup upgrade",
		CreatedAt:         1000,
	}, "#offrecord"); err != nil {
		t.Fatalf("upsert message: %v", err)
	}

	// Simulate out-of-order delivery: a message arrives on the new supergroup ID first.
	newChat, err := s.UpsertChat(ctx, Chat{
		TelegramChatID: -1009990001,
		Title:          "Backend Team",
		ChatType:       "supergroup",
	})
	if err != nil {
		t.Fatalf("upsert new chat: %v", err)
	}
	if _, err := s.UpsertMessage(ctx, Message{
		WorkspaceChatID:   newChat.WorkspaceChatID,
		TelegramMessageID: 11,
		SenderName:        "Bob",
		Text:              "First message after upgrade",
		CreatedAt:         2000,
	}, "#offrecord"); err != nil {
		t.Fatalf("upsert post-upgrade message: %v", err)
	}

	wsID, err := s.MigrateChat(ctx, -1001, -1009990001)
	if err != nil {
		t.Fatalf("migrate chat: %v", err)
	}
	if wsID != chat.WorkspaceChatID {
		t.Fatalf("expected original workspace_chat_id %s, got %s", chat.WorkspaceChatID, wsID)
	}

	// Both old and new Telegram chat IDs must resolve to the same workspace_chat_id.
	for _, tgID := range []int64{-1001, -1009990001} {
		resolved, err := s.ResolveWorkspaceChatID(ctx, tgID)
		if err != nil || resolved != wsID {
			t.Fatalf("resolve %d: got (%q, %v), want %q", tgID, resolved, err, wsID)
		}
	}

	if err := s.UpsertChatACL(ctx, ChatACL{
		WorkspaceChatID: wsID,
		TelegramUserID:  777,
		Status:          "member",
		IsMember:        true,
		CheckedAt:       2500,
	}); err != nil {
		t.Fatalf("upsert acl: %v", err)
	}

	msgs, err := s.GetMessages(ctx, MessageFilter{
		TelegramUserID:       777,
		WorkspaceChatID:      wsID,
		DefaultHistoryPolicy: config.HistoryPolicyFull,
	})
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 merged messages, got %d", len(msgs))
	}
}

func TestRBACAndHistoryPolicyFiltering(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	chat, err := s.UpsertChat(ctx, Chat{
		TelegramChatID: -100500,
		Title:          "Product",
		ChatType:       "supergroup",
	})
	if err != nil {
		t.Fatalf("upsert chat: %v", err)
	}

	// Message before user 200 joined, and message after user 200 joined.
	for _, m := range []Message{
		{
			WorkspaceChatID:   chat.WorkspaceChatID,
			TelegramMessageID: 1,
			SenderName:        "Alice",
			Text:              "Секретный архивный договор 2025",
			CreatedAt:         1000,
		},
		{
			WorkspaceChatID:   chat.WorkspaceChatID,
			TelegramMessageID: 2,
			SenderName:        "Bob",
			Text:              "Новый договор после онбординга",
			CreatedAt:         3000,
		},
	} {
		if _, err := s.UpsertMessage(ctx, m, "#offrecord"); err != nil {
			t.Fatalf("upsert message %d: %v", m.TelegramMessageID, err)
		}
	}

	// User 200 joined at timestamp 2000.
	if err := s.UpsertChatACL(ctx, ChatACL{
		WorkspaceChatID: chat.WorkspaceChatID,
		TelegramUserID:  200,
		Status:          "member",
		IsMember:        true,
		JoinedAt:        int64Ptr(2000),
		CheckedAt:       3500,
	}); err != nil {
		t.Fatalf("upsert acl 200: %v", err)
	}

	// User 300 is restricted with is_member=false (must have NO access).
	if err := s.UpsertChatACL(ctx, ChatACL{
		WorkspaceChatID: chat.WorkspaceChatID,
		TelegramUserID:  300,
		Status:          "restricted",
		IsMember:        false,
		CheckedAt:       3500,
	}); err != nil {
		t.Fatalf("upsert acl 300: %v", err)
	}

	// User 301 is restricted with is_member=true (must HAVE access).
	if err := s.UpsertChatACL(ctx, ChatACL{
		WorkspaceChatID: chat.WorkspaceChatID,
		TelegramUserID:  301,
		Status:          "restricted",
		IsMember:        true,
		CheckedAt:       3500,
	}); err != nil {
		t.Fatalf("upsert acl 301: %v", err)
	}

	// 1. Default policy = full_history -> user 200 sees both messages.
	fullMsgs, err := s.SearchMessages(ctx, SearchParams{
		TelegramUserID:       200,
		Query:                "договор",
		DefaultHistoryPolicy: config.HistoryPolicyFull,
	})
	if err != nil {
		t.Fatalf("search full_history: %v", err)
	}
	if len(fullMsgs) != 2 {
		t.Fatalf("expected 2 messages under full_history, got %d", len(fullMsgs))
	}

	// 2. Policy = since_join -> user 200 sees only message at t=3000.
	sinceMsgs, err := s.SearchMessages(ctx, SearchParams{
		TelegramUserID:       200,
		Query:                "договор",
		DefaultHistoryPolicy: config.HistoryPolicySinceJoin,
	})
	if err != nil {
		t.Fatalf("search since_join: %v", err)
	}
	if len(sinceMsgs) != 1 || sinceMsgs[0].TelegramMessageID != 2 {
		t.Fatalf("expected only msg 2 under since_join, got %+v", sinceMsgs)
	}

	// 3. Per-chat override overrides global full_history.
	override := config.HistoryPolicySinceJoin
	if err := s.SetChatHistoryPolicyOverride(ctx, chat.WorkspaceChatID, &override); err != nil {
		t.Fatalf("set chat override: %v", err)
	}
	overrideMsgs, err := s.GetMessages(ctx, MessageFilter{
		TelegramUserID:       200,
		WorkspaceChatID:      chat.WorkspaceChatID,
		DefaultHistoryPolicy: config.HistoryPolicyFull,
	})
	if err != nil {
		t.Fatalf("get messages with override: %v", err)
	}
	if len(overrideMsgs) != 1 || overrideMsgs[0].TelegramMessageID != 2 {
		t.Fatalf("expected chat override to restrict to msg 2, got %+v", overrideMsgs)
	}

	// 4. Restricted with is_member=false gets nothing in list_chats and search.
	chats300, err := s.ListAccessibleChats(ctx, 300, config.HistoryPolicyFull)
	if err != nil {
		t.Fatalf("list chats 300: %v", err)
	}
	if len(chats300) != 0 {
		t.Fatalf("restricted non-member must see 0 chats, got %d", len(chats300))
	}

	// 5. Restricted with is_member=true gets access.
	chats301, err := s.ListAccessibleChats(ctx, 301, config.HistoryPolicyFull)
	if err != nil {
		t.Fatalf("list chats 301: %v", err)
	}
	if len(chats301) != 1 {
		t.Fatalf("restricted active member must see 1 chat, got %d", len(chats301))
	}
}

func TestOffrecordAndTTLRetention(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	chat, err := s.UpsertChat(ctx, Chat{TelegramChatID: -200, Title: "Ops"})
	if err != nil {
		t.Fatalf("upsert chat: %v", err)
	}
	if err := s.UpsertChatACL(ctx, ChatACL{
		WorkspaceChatID: chat.WorkspaceChatID,
		TelegramUserID:  10,
		Status:          "administrator",
		IsMember:        true,
		CheckedAt:       time.Now().Unix(),
	}); err != nil {
		t.Fatalf("upsert acl: %v", err)
	}

	// #offrecord is ignored on insert (case-insensitive).
	ignored, err := s.UpsertMessage(ctx, Message{
		WorkspaceChatID:   chat.WorkspaceChatID,
		TelegramMessageID: 1,
		Text:              "Temp password 123 #OffRecord",
		CreatedAt:         time.Now().Unix(),
	}, "#offrecord")
	if err != nil || !ignored {
		t.Fatalf("expected offrecord message to be ignored, got (%v, %v)", ignored, err)
	}

	// Normal message later edited to add #offrecord is removed from DB and FTS.
	now := time.Now().UTC()
	if _, err := s.UpsertMessage(ctx, Message{
		WorkspaceChatID:   chat.WorkspaceChatID,
		TelegramMessageID: 2,
		Text:              "Sensitive token leak",
		CreatedAt:         now.Unix(),
	}, "#offrecord"); err != nil {
		t.Fatalf("insert msg 2: %v", err)
	}
	ignored, err = s.UpsertMessage(ctx, Message{
		WorkspaceChatID:   chat.WorkspaceChatID,
		TelegramMessageID: 2,
		Text:              "Sensitive token leak #offrecord",
		CreatedAt:         now.Unix(),
		EditedAt:          int64Ptr(now.Unix() + 5),
	}, "#offrecord")
	if err != nil || !ignored {
		t.Fatalf("expected edited offrecord to be removed, got (%v, %v)", ignored, err)
	}

	// Old message (100 days ago) vs fresh message (1 day ago).
	if _, err := s.UpsertMessage(ctx, Message{
		WorkspaceChatID:   chat.WorkspaceChatID,
		TelegramMessageID: 3,
		Text:              "Ancient deployment log",
		CreatedAt:         now.Add(-100 * 24 * time.Hour).Unix(),
	}, "#offrecord"); err != nil {
		t.Fatalf("insert old msg: %v", err)
	}
	if _, err := s.UpsertMessage(ctx, Message{
		WorkspaceChatID:   chat.WorkspaceChatID,
		TelegramMessageID: 4,
		Text:              "Recent deployment log",
		CreatedAt:         now.Add(-24 * time.Hour).Unix(),
	}, "#offrecord"); err != nil {
		t.Fatalf("insert recent msg: %v", err)
	}

	deleted, err := s.PurgeExpiredMessages(ctx, 90, now)
	if err != nil {
		t.Fatalf("purge expired: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 purged message, got %d", deleted)
	}

	found, err := s.SearchMessages(ctx, SearchParams{
		TelegramUserID:       10,
		Query:                "deployment",
		DefaultHistoryPolicy: config.HistoryPolicyFull,
	})
	if err != nil {
		t.Fatalf("search after purge: %v", err)
	}
	if len(found) != 1 || found[0].TelegramMessageID != 4 {
		t.Fatalf("expected only recent msg 4 after purge, got %+v", found)
	}
}

func TestGetThreadReplyChain(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	chat, err := s.UpsertChat(ctx, Chat{TelegramChatID: -300, Title: "Design"})
	if err != nil {
		t.Fatalf("upsert chat: %v", err)
	}
	if err := s.UpsertChatACL(ctx, ChatACL{
		WorkspaceChatID: chat.WorkspaceChatID,
		TelegramUserID:  50,
		Status:          "member",
		IsMember:        true,
		CheckedAt:       5000,
	}); err != nil {
		t.Fatalf("upsert acl: %v", err)
	}

	msgs := []Message{
		{WorkspaceChatID: chat.WorkspaceChatID, TelegramMessageID: 10, Text: "Root question", CreatedAt: 100},
		{WorkspaceChatID: chat.WorkspaceChatID, TelegramMessageID: 11, Text: "Unrelated message", CreatedAt: 150},
		{WorkspaceChatID: chat.WorkspaceChatID, TelegramMessageID: 12, Text: "First reply", ReplyToMessageID: int64Ptr(10), CreatedAt: 200},
		{WorkspaceChatID: chat.WorkspaceChatID, TelegramMessageID: 13, Text: "Nested reply", ReplyToMessageID: int64Ptr(12), CreatedAt: 300},
	}
	for _, m := range msgs {
		if _, err := s.UpsertMessage(ctx, m, ""); err != nil {
			t.Fatalf("upsert msg %d: %v", m.TelegramMessageID, err)
		}
	}

	// Querying from the middle message (12) must return the full chain [10, 12, 13].
	thread, err := s.GetThread(ctx, ThreadParams{
		TelegramUserID:       50,
		WorkspaceChatID:      chat.WorkspaceChatID,
		TelegramMessageID:    12,
		DefaultHistoryPolicy: config.HistoryPolicyFull,
	})
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if len(thread) != 3 || thread[0].TelegramMessageID != 10 || thread[1].TelegramMessageID != 12 || thread[2].TelegramMessageID != 13 {
		t.Fatalf("unexpected thread: %+v", thread)
	}
}

func TestAuthTokensLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	tok1, _, err := s.IssueAuthToken(ctx, 42, 24*time.Hour)
	if err != nil {
		t.Fatalf("issue token 1: %v", err)
	}

	uid, err := s.ValidateAuthToken(ctx, tok1, time.Now())
	if err != nil || uid != 42 {
		t.Fatalf("expected valid token for user 42, got (%d, %v)", uid, err)
	}

	// Re-issuing /auth must revoke the previous token.
	tok2, _, err := s.IssueAuthToken(ctx, 42, 24*time.Hour)
	if err != nil {
		t.Fatalf("issue token 2: %v", err)
	}
	if _, err := s.ValidateAuthToken(ctx, tok1, time.Now()); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected tok1 to be revoked, got %v", err)
	}
	if uid, err := s.ValidateAuthToken(ctx, tok2, time.Now()); err != nil || uid != 42 {
		t.Fatalf("expected tok2 valid, got (%d, %v)", uid, err)
	}

	// Expired check.
	if _, err := s.ValidateAuthToken(ctx, tok2, time.Now().Add(48*time.Hour)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected expired tok2 to fail, got %v", err)
	}

	// Admin global revoke.
	revoked, err := s.RevokeUserTokens(ctx, 42)
	if err != nil || revoked != 1 {
		t.Fatalf("expected 1 revoked token, got (%d, %v)", revoked, err)
	}
	if _, err := s.ValidateAuthToken(ctx, tok2, time.Now()); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected revoked tok2 to fail, got %v", err)
	}

	// Non-expiring token (ttl == 0) remains valid far in the future until revoked.
	tokForever, recForever, err := s.IssueAuthToken(ctx, 42, 0)
	if err != nil || recForever.ExpiresAt != 0 {
		t.Fatalf("issue non-expiring token: (%+v, %v)", recForever, err)
	}
	if uid, err := s.ValidateAuthToken(ctx, tokForever, time.Now().Add(10*365*24*time.Hour)); err != nil || uid != 42 {
		t.Fatalf("expected non-expiring token to stay valid, got (%d, %v)", uid, err)
	}
}

func TestBuildFTSQueryPunctuationSafety(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	chat, _ := s.UpsertChat(ctx, Chat{TelegramChatID: -400, Title: "Tickets"})
	_ = s.UpsertChatACL(ctx, ChatACL{
		WorkspaceChatID: chat.WorkspaceChatID,
		TelegramUserID:  1,
		Status:          "member",
		IsMember:        true,
		CheckedAt:       100,
	})
	_, _ = s.UpsertMessage(ctx, Message{
		WorkspaceChatID:   chat.WorkspaceChatID,
		TelegramMessageID: 1,
		Text:              "Fixed PROJ-412: timeout on /mcp endpoint",
		CreatedAt:         100,
	}, "")

	// Query with punctuation that would break raw FTS5 MATCH syntax.
	res, err := s.SearchMessages(ctx, SearchParams{
		TelegramUserID:       1,
		Query:                `PROJ-412: "/mcp"*`,
		DefaultHistoryPolicy: config.HistoryPolicyFull,
	})
	if err != nil {
		t.Fatalf("punctuated search failed: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("expected 1 result, got %d", len(res))
	}
}

func TestDailyDigestsRBACAndSinceJoin(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	chat, err := s.UpsertChat(ctx, Chat{TelegramChatID: -500, Title: "Product"})
	if err != nil {
		t.Fatalf("upsert chat: %v", err)
	}

	// Day 1: [1000..1999], Day 2: [2000..2999]
	if err := s.UpsertDigest(ctx, DailyDigest{
		WorkspaceChatID: chat.WorkspaceChatID,
		DigestDate:      "2026-11-20",
		PeriodStart:     1000,
		PeriodEnd:       1999,
		MessageCount:    12,
		LastMessageID:   12,
		Summary:         "Day 1 summary",
		KeyDecisions:    []string{"Ship alpha"},
		ActionItems:     []string{"Alice: QA"},
		Topics:          []string{"alpha"},
		Model:           "qwen2.5:7b",
	}); err != nil {
		t.Fatalf("upsert digest day 1: %v", err)
	}
	if err := s.UpsertDigest(ctx, DailyDigest{
		WorkspaceChatID: chat.WorkspaceChatID,
		DigestDate:      "2026-11-21",
		PeriodStart:     2000,
		PeriodEnd:       2999,
		MessageCount:    8,
		LastMessageID:   20,
		Summary:         "Day 2 summary",
		Model:           "qwen2.5:7b",
	}); err != nil {
		t.Fatalf("upsert digest day 2: %v", err)
	}

	// User 10 joined mid-way through Day 1 (t=1500).
	_ = s.UpsertChatACL(ctx, ChatACL{
		WorkspaceChatID: chat.WorkspaceChatID,
		TelegramUserID:  10,
		Status:          "member",
		IsMember:        true,
		JoinedAt:        int64Ptr(1500),
		CheckedAt:       5000,
	})

	// Under full_history -> sees both digests.
	all, err := s.GetDigests(ctx, DigestFilter{
		TelegramUserID:       10,
		WorkspaceChatID:      chat.WorkspaceChatID,
		DefaultHistoryPolicy: config.HistoryPolicyFull,
		MinACLCheckedAt:      4900,
	})
	if err != nil || len(all) != 2 {
		t.Fatalf("expected 2 digests under full_history, got (%d, %v)", len(all), err)
	}

	// Under since_join -> Day 1 started at 1000 < 1500 so ONLY Day 2 is visible (no pre-join leak!).
	since, err := s.GetDigests(ctx, DigestFilter{
		TelegramUserID:       10,
		WorkspaceChatID:      chat.WorkspaceChatID,
		DefaultHistoryPolicy: config.HistoryPolicySinceJoin,
		MinACLCheckedAt:      4900,
	})
	if err != nil || len(since) != 1 || since[0].DigestDate != "2026-11-21" {
		t.Fatalf("expected only Day 2 under since_join, got %+v (err=%v)", since, err)
	}

	// Stale ACL (checked_at < MinACLCheckedAt) -> fail-closed (0 results).
	stale, err := s.GetDigests(ctx, DigestFilter{
		TelegramUserID:       10,
		WorkspaceChatID:      chat.WorkspaceChatID,
		DefaultHistoryPolicy: config.HistoryPolicyFull,
		MinACLCheckedAt:      6000,
	})
	if err != nil || len(stale) != 0 {
		t.Fatalf("expected 0 digests when ACL is stale, got (%d, %v)", len(stale), err)
	}
}
