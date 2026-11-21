package rbac

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mytischi/mcpgram/internal/config"
	"github.com/mytischi/mcpgram/internal/storage"
	"github.com/mytischi/mcpgram/internal/telegram"
)

// MemberChecker is the Telegram Bot API subset needed for live membership checks.
type MemberChecker interface {
	GetChatMember(ctx context.Context, chatID, userID int64) (telegram.ChatMember, error)
}

// Authorizer enforces hybrid Push + Pull RBAC backed by chat_acl and live Telegram getChatMember.
type Authorizer struct {
	store    *storage.Store
	checker  MemberChecker
	cacheTTL time.Duration
	nowFunc  func() time.Time
	logger   *slog.Logger
}

// NewAuthorizer creates a hybrid RBAC Authorizer.
func NewAuthorizer(store *storage.Store, checker MemberChecker, cacheTTL time.Duration, logger *slog.Logger) *Authorizer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Authorizer{
		store:    store,
		checker:  checker,
		cacheTTL: cacheTTL,
		nowFunc:  time.Now,
		logger:   logger,
	}
}

// MinFreshCheckedAt returns the oldest acceptable chat_acl.checked_at timestamp for fail-closed SQL queries.
func (a *Authorizer) MinFreshCheckedAt() int64 {
	return a.nowFunc().UTC().Add(-a.cacheTTL).Unix()
}

// CheckChatAccess verifies whether telegramUserID currently has read access to workspaceChatID.
// Uses chat_acl when fresh within cacheTTL; otherwise performs a live getChatMember call.
// On transient Telegram errors when the cache is missing or stale, fails closed (returns false, err).
func (a *Authorizer) CheckChatAccess(ctx context.Context, workspaceChatID string, telegramUserID int64) (bool, error) {
	chat, err := a.store.GetChatByWorkspaceID(ctx, workspaceChatID)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load chat for rbac: %w", err)
	}

	// If the bot is no longer in this chat, deny access (cannot verify live membership).
	if chat.BotStatus == "left" || chat.BotStatus == "kicked" {
		return false, nil
	}

	nowUnix := a.nowFunc().UTC().Unix()
	minFresh := a.MinFreshCheckedAt()

	acl, err := a.store.GetChatACL(ctx, workspaceChatID, telegramUserID)
	if err == nil && acl.CheckedAt >= minFresh {
		return storage.IsAllowedMemberStatus(acl.Status, acl.IsMember), nil
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return false, fmt.Errorf("load chat_acl: %w", err)
	}

	// Cache missing or stale -> live check via Telegram Bot API.
	cm, err := a.checker.GetChatMember(ctx, chat.TelegramChatID, telegramUserID)
	if err != nil {
		if telegram.IsNotParticipantError(err) {
			_ = a.store.UpsertChatACL(ctx, storage.ChatACL{
				WorkspaceChatID: workspaceChatID,
				TelegramUserID:  telegramUserID,
				Status:          "left",
				IsMember:        false,
				CheckedAt:       nowUnix,
			})
			return false, nil
		}
		// Strict fail-closed: never trust stale ACL on network/API failure.
		return false, fmt.Errorf("live getChatMember failed (fail-closed): %w", err)
	}

	isMember := cm.EffectiveIsMember()
	if err := a.store.UpsertChatACL(ctx, storage.ChatACL{
		WorkspaceChatID: workspaceChatID,
		TelegramUserID:  telegramUserID,
		Status:          cm.Status,
		IsMember:        isMember,
		CheckedAt:       nowUnix,
	}); err != nil {
		return false, fmt.Errorf("update chat_acl cache: %w", err)
	}

	return storage.IsAllowedMemberStatus(cm.Status, isMember), nil
}

// RefreshUserChats ensures chat_acl entries are fresh across all tracked active chats for telegramUserID.
// Transient errors for individual chats are logged and left stale so SQL MinACLCheckedAt excludes them (fail-closed).
func (a *Authorizer) RefreshUserChats(ctx context.Context, telegramUserID int64) error {
	chats, err := a.store.ListAllChats(ctx)
	if err != nil {
		return fmt.Errorf("list chats for rbac refresh: %w", err)
	}

	for _, c := range chats {
		if c.BotStatus == "left" || c.BotStatus == "kicked" {
			continue
		}
		if _, err := a.CheckChatAccess(ctx, c.WorkspaceChatID, telegramUserID); err != nil {
			a.logger.Warn("rbac live check failed for chat; failing closed for this chat",
				"workspace_chat_id", c.WorkspaceChatID,
				"telegram_user_id", telegramUserID,
				"error", err,
			)
		}
	}
	return nil
}

// ListAccessibleChats refreshes stale ACLs and returns only freshly verified accessible chats.
func (a *Authorizer) ListAccessibleChats(ctx context.Context, telegramUserID int64, defaultPolicy config.HistoryPolicy) ([]storage.ChatSummary, error) {
	if err := a.RefreshUserChats(ctx, telegramUserID); err != nil {
		return nil, err
	}
	return a.store.ListAccessibleChats(ctx, telegramUserID, defaultPolicy, a.MinFreshCheckedAt())
}

// HasAnyTrackedChatAccess reports whether telegramUserID is currently a verified member of
// at least one tracked corporate chat (used to guard /auth against outsiders).
func (a *Authorizer) HasAnyTrackedChatAccess(ctx context.Context, telegramUserID int64) (bool, error) {
	chats, err := a.store.ListAllChats(ctx)
	if err != nil {
		return false, fmt.Errorf("list chats for /auth check: %w", err)
	}

	var lastErr error
	for _, c := range chats {
		if c.BotStatus == "left" || c.BotStatus == "kicked" {
			continue
		}
		allowed, err := a.CheckChatAccess(ctx, c.WorkspaceChatID, telegramUserID)
		if err != nil {
			lastErr = err
			continue
		}
		if allowed {
			return true, nil
		}
	}
	return false, lastErr
}
