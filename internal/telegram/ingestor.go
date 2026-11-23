package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/mytischi/mcpgram/internal/config"
	"github.com/mytischi/mcpgram/internal/importer"
	"github.com/mytischi/mcpgram/internal/storage"
)

const (
	downtimeAlertThreshold = 20 * time.Hour
	maxBotImportFileBytes  = 20 << 20 // 20 MiB Telegram Bot API getFile limit
	callbackHideToken      = "hide_token"
)

// AllowedUpdates explicitly requests chat_member, my_chat_member, and callback_query events
// (Telegram omits chat_member unless listed explicitly).
var AllowedUpdates = []string{
	"message",
	"edited_message",
	"chat_member",
	"my_chat_member",
	"callback_query",
}

// AccessChecker verifies whether a Telegram user belongs to at least one tracked corporate chat.
type AccessChecker interface {
	HasAnyTrackedChatAccess(ctx context.Context, telegramUserID int64) (bool, error)
}

// DigestTrigger allows admin /digest commands to invoke nightly digest generation on demand.
type DigestTrigger interface {
	Enabled() bool
	RunPending(ctx context.Context) (generated, skipped, failed int, err error)
	RunForDate(ctx context.Context, dateStr string, force bool) (generated, skipped, failed int, err error)
	TodayDate() string
}

// OAuthApprover connects Telegram /start login_<nonce> confirmations to the MCP OAuth 2.1 server.
type OAuthApprover interface {
	ApproveLogin(ctx context.Context, nonce string, telegramUserID int64) error
	DenyLogin(ctx context.Context, nonce string) error
}

// Ingestor polls Telegram updates, stores group messages/topics/ACL, and handles DM bot commands.
type Ingestor struct {
	api           API
	store         *storage.Store
	cfg           config.Config
	access        AccessChecker
	digestRunner  DigestTrigger
	oauthApprover OAuthApprover
	logger        *slog.Logger
	pollTimeout   int
	botID         int64
	botUsername   string
	canReadAll    bool
	nowFunc       func() time.Time
}

// NewIngestor creates an Ingestor bound to a Telegram API client, storage, config, and RBAC checker.
func NewIngestor(api API, store *storage.Store, cfg config.Config, access AccessChecker, logger *slog.Logger) *Ingestor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Ingestor{
		api:         api,
		store:       store,
		cfg:         cfg,
		access:      access,
		logger:      logger,
		pollTimeout: 30,
		nowFunc:     time.Now,
	}
}

// SetDigestRunner attaches an optional DigestTrigger for the admin /digest command.
func (i *Ingestor) SetDigestRunner(runner DigestTrigger) {
	i.digestRunner = runner
}

// SetOAuthApprover attaches an optional OAuthApprover for /start login_<nonce> confirmations.
func (i *Ingestor) SetOAuthApprover(approver OAuthApprover) {
	i.oauthApprover = approver
}

// Bootstrap runs getMe diagnostics, persists bot metadata, registers private bot commands,
// and notifies admins if extended server downtime (>20h) was detected.
func (i *Ingestor) Bootstrap(ctx context.Context) (int64, error) {
	me, err := i.api.GetMe(ctx)
	if err != nil {
		return 0, fmt.Errorf("telegram getMe failed: %w", err)
	}
	i.botID = me.ID
	i.botUsername = me.Username
	i.canReadAll = me.CanReadAllGroupMessages

	if err := i.store.UpsertBot(ctx, storage.Bot{
		BotID:                   me.ID,
		Username:                me.Username,
		FirstName:               me.FirstName,
		CanReadAllGroupMessages: me.CanReadAllGroupMessages,
	}); err != nil {
		return 0, fmt.Errorf("persist bot metadata: %w", err)
	}

	if !me.CanReadAllGroupMessages {
		i.logger.Warn("bot Privacy Mode appears ENABLED (can_read_all_group_messages=false): "+
			"disable via @BotFather -> /setprivacy -> Disable and grant admin status in groups",
			"bot_username", me.Username,
		)
	} else {
		i.logger.Info("telegram bot verified",
			"bot_id", me.ID,
			"bot_username", me.Username,
			"can_read_all_group_messages", true,
		)
	}

	commands := []BotCommand{
		{Command: "connect", Description: "Get setup instructions & ready prompt for your AI agent"},
		{Command: "auth", Description: "Issue a manual Bearer token for CLI/non-OAuth MCP clients"},
		{Command: "status", Description: "[Admin] Check bot health, workers, and tracked chats"},
		{Command: "digest", Description: "[Admin] Trigger daily digest generation on demand"},
		{Command: "set_policy", Description: "[Admin] Override chat history policy (full_history|since_join|default)"},
		{Command: "revoke_user", Description: "[Admin] Revoke all tokens for a Telegram user ID"},
	}
	if err := i.api.SetMyCommands(ctx, commands); err != nil {
		i.logger.Warn("failed to set bot commands menu", "error", err)
	}

	botRec, err := i.store.GetBot(ctx, me.ID)
	if err != nil {
		return 0, err
	}

	i.checkDowntimeAlert(ctx, botRec)
	return botRec.UpdateOffset, nil
}

func (i *Ingestor) checkDowntimeAlert(ctx context.Context, botRec storage.Bot) {
	if botRec.LastPollAt == nil || *botRec.LastPollAt <= 0 {
		return
	}
	lastPoll := time.Unix(*botRec.LastPollAt, 0).UTC()
	now := i.nowFunc().UTC()
	downtime := now.Sub(lastPoll)
	if downtime < downtimeAlertThreshold {
		return
	}

	hours := int(downtime.Hours())
	i.logger.Warn("detected extended server downtime exceeding 20h; Telegram update queue may have dropped messages",
		"downtime_hours", hours,
		"last_poll_at", lastPoll.Format(time.RFC3339),
	)

	alert := fmt.Sprintf(
		"⚠️ Extended server downtime detected: ~%dh (from %s to %s UTC).\n"+
			"Telegram Bot API keeps queued updates for 24 hours, so some messages during this window may have been missed.\n\n"+
			"To backfill the gap without duplicates:\n"+
			"1. Export the affected chat in Telegram Desktop (JSON result.json) or Telegram for macOS (HTML messages.html), unchecking media.\n"+
			"2. Send the exported result.json or messages.html file directly to me in this chat (up to 20 MiB).",
		hours,
		lastPoll.Format("2006-01-02 15:04"),
		now.Format("2006-01-02 15:04"),
	)
	for _, adminID := range i.cfg.AdminTGIDs {
		if err := i.api.SendMessage(ctx, adminID, alert); err != nil {
			i.logger.Warn("failed to send downtime alert to admin", "admin_id", adminID, "error", err)
		}
	}
}

// Run bootstraps the bot and runs the Long Polling loop until ctx is canceled.
func (i *Ingestor) Run(ctx context.Context) error {
	offset, err := i.Bootstrap(ctx)
	if err != nil {
		return err
	}

	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		updates, err := i.api.GetUpdates(ctx, offset, i.pollTimeout, AllowedUpdates)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			i.logger.Error("telegram getUpdates error; backing off",
				"error", err,
				"backoff", backoff.String(),
			)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		backoff = time.Second
		for _, upd := range updates {
			if err := i.HandleUpdate(ctx, upd); err != nil {
				i.logger.Error("handle update failed",
					"update_id", upd.UpdateID,
					"error", err,
				)
			}
			if upd.UpdateID+1 > offset {
				offset = upd.UpdateID + 1
			}
		}

		if err := i.store.UpdateBotOffset(ctx, i.botID, offset, i.nowFunc().UTC().Unix()); err != nil {
			i.logger.Error("persist update offset failed", "error", err)
		}
	}
}

// HandleUpdate dispatches a single Telegram update to the appropriate handler.
func (i *Ingestor) HandleUpdate(ctx context.Context, upd Update) error {
	switch {
	case upd.CallbackQuery != nil:
		return i.handleCallbackQuery(ctx, upd.CallbackQuery)
	case upd.MyChatMember != nil:
		return i.handleMyChatMember(ctx, upd.MyChatMember)
	case upd.ChatMember != nil:
		return i.handleChatMember(ctx, upd.ChatMember)
	case upd.Message != nil:
		return i.handleMessage(ctx, upd.Message, false)
	case upd.EditedMessage != nil:
		return i.handleMessage(ctx, upd.EditedMessage, true)
	default:
		return nil
	}
}

func (i *Ingestor) handleCallbackQuery(ctx context.Context, cq *CallbackQuery) error {
	if cq.Data == callbackHideToken && cq.Message != nil {
		if err := i.api.EditMessageText(ctx, cq.Message.Chat.ID, cq.Message.MessageID,
			"🔒 Token hidden. Run /auth anytime to issue a new token.",
		); err != nil {
			i.logger.Warn("failed to hide token message", "error", err)
		}
		return i.api.AnswerCallbackQuery(ctx, cq.ID, "Token hidden")
	}
	return i.api.AnswerCallbackQuery(ctx, cq.ID, "")
}

func (i *Ingestor) handleMyChatMember(ctx context.Context, ev *ChatMemberUpdated) error {
	if ev.Chat.Type != "group" && ev.Chat.Type != "supergroup" {
		return nil
	}

	status := strings.ToLower(strings.TrimSpace(ev.NewChatMember.Status))
	isAdmin := status == "administrator" || status == "creator"

	var botIDPtr *int64
	if i.botID != 0 {
		v := i.botID
		botIDPtr = &v
	}

	_, err := i.store.UpsertChat(ctx, storage.Chat{
		TelegramChatID: ev.Chat.ID,
		AssignedBotID:  botIDPtr,
		Title:          ev.Chat.Title,
		ChatType:       ev.Chat.Type,
		IsForum:        ev.Chat.IsForum,
		BotIsAdmin:     isAdmin,
		BotStatus:      status,
	})
	if err != nil {
		return fmt.Errorf("upsert chat on my_chat_member: %w", err)
	}

	if !isAdmin && status != "left" && status != "kicked" {
		i.logger.Warn("bot is in group without administrator rights; RBAC chat_member events will not arrive",
			"telegram_chat_id", ev.Chat.ID,
			"title", ev.Chat.Title,
			"bot_status", status,
		)
	}
	return nil
}

func (i *Ingestor) handleChatMember(ctx context.Context, ev *ChatMemberUpdated) error {
	if ev.Chat.Type != "group" && ev.Chat.Type != "supergroup" {
		return nil
	}

	var botIDPtr *int64
	if i.botID != 0 {
		v := i.botID
		botIDPtr = &v
	}

	chat, err := i.store.UpsertChat(ctx, storage.Chat{
		TelegramChatID: ev.Chat.ID,
		AssignedBotID:  botIDPtr,
		Title:          ev.Chat.Title,
		ChatType:       ev.Chat.Type,
		IsForum:        ev.Chat.IsForum,
	})
	if err != nil {
		return fmt.Errorf("upsert chat on chat_member: %w", err)
	}

	oldAllowed := storage.IsAllowedMemberStatus(ev.OldChatMember.Status, ev.OldChatMember.EffectiveIsMember())
	newAllowed := storage.IsAllowedMemberStatus(ev.NewChatMember.Status, ev.NewChatMember.EffectiveIsMember())

	var joinedAt *int64
	if !oldAllowed && newAllowed {
		ts := ev.Date
		if ts == 0 {
			ts = i.nowFunc().UTC().Unix()
		}
		joinedAt = &ts
	}

	return i.store.UpsertChatACL(ctx, storage.ChatACL{
		WorkspaceChatID: chat.WorkspaceChatID,
		TelegramUserID:  ev.NewChatMember.User.ID,
		Status:          ev.NewChatMember.Status,
		IsMember:        ev.NewChatMember.EffectiveIsMember(),
		JoinedAt:        joinedAt,
		CheckedAt:       i.nowFunc().UTC().Unix(),
	})
}

func (i *Ingestor) handleMessage(ctx context.Context, msg *Message, edited bool) error {
	if msg.Chat.Type == "private" {
		if edited {
			return nil
		}
		if msg.Document != nil {
			return i.handlePrivateDocument(ctx, msg)
		}
		return i.handlePrivateCommand(ctx, msg)
	}

	if msg.Chat.Type != "group" && msg.Chat.Type != "supergroup" {
		return nil
	}

	if msg.MigrateToChatID != 0 {
		if _, err := i.store.MigrateChat(ctx, msg.Chat.ID, msg.MigrateToChatID); err != nil {
			return fmt.Errorf("migrate chat %d -> %d: %w", msg.Chat.ID, msg.MigrateToChatID, err)
		}
		return nil
	}
	if msg.MigrateFromChatID != 0 {
		if _, err := i.store.MigrateChat(ctx, msg.MigrateFromChatID, msg.Chat.ID); err != nil {
			return fmt.Errorf("migrate chat %d -> %d: %w", msg.MigrateFromChatID, msg.Chat.ID, err)
		}
	}

	var botIDPtr *int64
	if i.botID != 0 {
		v := i.botID
		botIDPtr = &v
	}

	chat, err := i.store.UpsertChat(ctx, storage.Chat{
		TelegramChatID: msg.Chat.ID,
		AssignedBotID:  botIDPtr,
		Title:          msg.Chat.Title,
		ChatType:       msg.Chat.Type,
		IsForum:        msg.Chat.IsForum,
	})
	if err != nil {
		return fmt.Errorf("upsert chat on message: %w", err)
	}

	if msg.MessageThreadID != nil {
		if msg.ForumTopicCreated != nil && msg.ForumTopicCreated.Name != "" {
			if err := i.store.UpsertTopic(ctx, storage.Topic{
				WorkspaceChatID: chat.WorkspaceChatID,
				MessageThreadID: *msg.MessageThreadID,
				Title:           msg.ForumTopicCreated.Name,
			}); err != nil {
				return err
			}
		}
		if msg.ForumTopicEdited != nil && msg.ForumTopicEdited.Name != "" {
			if err := i.store.UpsertTopic(ctx, storage.Topic{
				WorkspaceChatID: chat.WorkspaceChatID,
				MessageThreadID: *msg.MessageThreadID,
				Title:           msg.ForumTopicEdited.Name,
			}); err != nil {
				return err
			}
		}
	}

	text := msg.EffectiveText()
	attachMeta := msg.ExtractAttachmentMeta()
	if text == "" && attachMeta == nil {
		return nil
	}

	var senderID *int64
	var senderName, senderUsername string
	if msg.From != nil {
		id := msg.From.ID
		senderID = &id
		senderName = msg.From.DisplayName()
		senderUsername = msg.From.Username
	}

	var replyTo *int64
	if msg.ReplyToMessage != nil {
		rid := msg.ReplyToMessage.MessageID
		replyTo = &rid
	}

	editedAt := msg.EditDate
	if edited && editedAt == nil {
		ts := i.nowFunc().UTC().Unix()
		editedAt = &ts
	}

	_, err = i.store.UpsertMessage(ctx, storage.Message{
		WorkspaceChatID:   chat.WorkspaceChatID,
		TelegramMessageID: msg.MessageID,
		SenderTGID:        senderID,
		SenderName:        senderName,
		SenderUsername:    senderUsername,
		Text:              text,
		ReplyToMessageID:  replyTo,
		MessageThreadID:   msg.MessageThreadID,
		AttachmentMeta:    attachMeta,
		CreatedAt:         msg.Date,
		EditedAt:          editedAt,
	}, i.cfg.IgnoreTag)
	return err
}

func (i *Ingestor) handlePrivateCommand(ctx context.Context, msg *Message) error {
	if msg.From == nil {
		return nil
	}
	userID := msg.From.ID
	raw := strings.TrimSpace(msg.Text)
	if !strings.HasPrefix(raw, "/") {
		return nil
	}

	fields := strings.Fields(raw)
	cmd := strings.ToLower(fields[0])
	if idx := strings.Index(cmd, "@"); idx != -1 {
		cmd = cmd[:idx]
	}

	switch cmd {
	case "/start":
		if len(fields) >= 2 && strings.HasPrefix(fields[1], "login_") {
			nonce := strings.TrimPrefix(fields[1], "login_")
			return i.cmdOAuthConfirm(ctx, msg.Chat.ID, userID, nonce)
		}
		return i.cmdOnboarding(ctx, msg.Chat.ID, userID)

	case "/connect", "/help":
		return i.cmdOnboarding(ctx, msg.Chat.ID, userID)

	case "/auth":
		return i.cmdAuth(ctx, msg.Chat.ID, userID)

	case "/status":
		return i.cmdStatus(ctx, msg.Chat.ID, userID)

	case "/digest":
		return i.cmdDigest(ctx, msg.Chat.ID, userID, fields[1:])

	case "/set_policy":
		return i.cmdSetPolicy(ctx, msg.Chat.ID, userID, fields[1:])

	case "/revoke_user":
		return i.cmdRevokeUser(ctx, msg.Chat.ID, userID, fields[1:])

	default:
		return nil
	}
}

func (i *Ingestor) isWorkspaceMember(ctx context.Context, userID int64) (bool, error) {
	if i.cfg.IsAdmin(userID) {
		return true, nil
	}
	if i.access == nil {
		return false, nil
	}
	return i.access.HasAnyTrackedChatAccess(ctx, userID)
}

func (i *Ingestor) cmdOAuthConfirm(ctx context.Context, chatID, userID int64, nonce string) error {
	if i.oauthApprover == nil || !i.cfg.OAuthEnabled {
		return i.api.SendMessage(ctx, chatID, "MCP OAuth 2.1 is not enabled on this server. Use /auth to obtain a Bearer token.")
	}

	allowed, err := i.isWorkspaceMember(ctx, userID)
	if err != nil {
		i.logger.Error("oauth membership check failed", "user_id", userID, "error", err)
		return i.api.SendMessage(ctx, chatID, "Could not verify your workspace chat membership right now. Please try again shortly.")
	}

	if !allowed {
		_ = i.oauthApprover.DenyLogin(ctx, nonce)
		return i.api.SendMessage(ctx, chatID,
			fmt.Sprintf("❌ Access denied (Telegram ID: %d). You are not a member of any tracked workspace chat.", userID),
		)
	}

	if err := i.oauthApprover.ApproveLogin(ctx, nonce, userID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return i.api.SendMessage(ctx, chatID, "⚠️ This sign-in link has expired or was already used. Please restart sign-in from your MCP client.")
		}
		return fmt.Errorf("approve oauth login: %w", err)
	}

	return i.api.SendMessage(ctx, chatID, "✅ Sign-in approved! You can return to your browser and MCP client now.")
}

func (i *Ingestor) cmdOnboarding(ctx context.Context, chatID, userID int64) error {
	allowed, err := i.isWorkspaceMember(ctx, userID)
	if err != nil {
		i.logger.Error("onboarding membership check failed", "user_id", userID, "error", err)
		return i.api.SendMessage(ctx, chatID,
			fmt.Sprintf("Your Telegram ID: %d\n\nCould not verify workspace membership right now. Please try again later.", userID),
		)
	}

	// Never leak PUBLIC_BASE_URL or setup prompts to outsiders not in any tracked chat.
	if !allowed {
		return i.api.SendMessage(ctx, chatID,
			fmt.Sprintf("Your Telegram ID: %d\n\nAccess denied: you are not a member of any tracked workspace chat. Ask an administrator to add you to a workspace group first.", userID),
		)
	}

	mcpURL := i.mcpEndpointURL()
	serverName := i.cfg.MCPServerName

	var b strings.Builder
	fmt.Fprintf(&b, "Welcome to mcpgram! (Telegram ID: %d)\n\n", userID)

	if i.cfg.OAuthEnabled && i.cfg.PublicBaseURL != "" {
		fmt.Fprintf(&b, "Quick setup with MCP OAuth 2.1 (no manual tokens needed):\n")
		fmt.Fprintf(&b, "1. Copy and send this prompt to your AI agent (Cursor, Claude Code, OpenCode, etc.):\n\n")
		fmt.Fprintf(&b, "Connect the remote Streamable HTTP MCP server named %q at %s using standard MCP OAuth 2.1 (no manual Bearer token needed).\n\n", serverName, mcpURL)
		fmt.Fprintf(&b, "2. When your client opens the authorization link, click \"Confirm in Telegram\" and press Start here.\n")
		fmt.Fprintf(&b, "3. Done — your client saves credentials securely and refreshes them automatically.\n\n")
		fmt.Fprintf(&b, "Using a CLI client without OAuth? Run /auth to get a manual Bearer token.")
	} else {
		fmt.Fprintf(&b, "Quick setup:\n")
		fmt.Fprintf(&b, "1. Send this safe prompt (no secrets!) to your AI agent to add the config entry:\n\n")
		fmt.Fprintf(&b, "Add a remote Streamable HTTP MCP server named %q at %s with header \"Authorization: Bearer PASTE_TOKEN_HERE\", then open the config file for me.\n\n", serverName, mcpURL)
		fmt.Fprintf(&b, "2. Run /auth here to get your personal Bearer token and replace PASTE_TOKEN_HERE yourself.\n")
		fmt.Fprintf(&b, "Never paste your token directly into an AI chat.")
	}

	if i.cfg.IsAdmin(userID) {
		fmt.Fprintf(&b, "\n\nAdmin commands:\n")
		fmt.Fprintf(&b, "• /status — server diagnostics & tracked chats\n")
		fmt.Fprintf(&b, "• /digest [YYYY-MM-DD|today] — trigger daily digests\n")
		fmt.Fprintf(&b, "• /set_policy <ws_id> <full_history|since_join|default> — per-chat history override\n")
		fmt.Fprintf(&b, "• /revoke_user <telegram_user_id> — immediately revoke all user tokens\n")
		fmt.Fprintf(&b, "• Send a Telegram export file (result.json or messages.html) here to import chat history")
	}

	return i.api.SendMessage(ctx, chatID, b.String())
}

func (i *Ingestor) mcpEndpointURL() string {
	if i.cfg.PublicBaseURL != "" {
		return i.cfg.PublicBaseURL + "/mcp"
	}
	return "http://localhost:8080/mcp"
}

func (i *Ingestor) cmdAuth(ctx context.Context, chatID, userID int64) error {
	allowed, err := i.isWorkspaceMember(ctx, userID)
	if err != nil {
		i.logger.Error("auth membership check failed", "user_id", userID, "error", err)
		return i.api.SendMessage(ctx, chatID, "Could not verify your workspace chat membership. Please try again later.")
	}

	if !allowed {
		return i.api.SendMessage(ctx, chatID,
			fmt.Sprintf("Access denied (Telegram ID: %d). You are not a member of any tracked workspace chat.", userID),
		)
	}

	ttl := time.Duration(i.cfg.TokenTTLDays) * 24 * time.Hour
	rawToken, rec, err := i.store.IssueAuthToken(ctx, userID, ttl)
	if err != nil {
		return fmt.Errorf("issue auth token: %w", err)
	}

	validity := "no expiration, until revoked"
	if rec.ExpiresAt > 0 {
		validity = "valid until " + time.Unix(rec.ExpiresAt, 0).UTC().Format("2006-01-02")
	}
	mcpURL := i.mcpEndpointURL()
	serverName := i.cfg.MCPServerName

	reply := fmt.Sprintf(
		"Telegram ID: %d\n"+
			"Personal Bearer token (%s; previous manual token revoked):\n\n"+
			"%s\n\n"+
			"MCP client config (mcp.json):\n"+
			"{\"mcpServers\":{%q:{\"url\":%q,\"headers\":{\"Authorization\":\"Bearer %s\"}}}}\n\n"+
			"Claude Code CLI:\n"+
			"claude mcp add --transport http %s %s --header \"Authorization: Bearer %s\"\n\n"+
			"Security note: never paste this token into an AI chat. Click \"Hide token\" below once copied.",
		userID, validity, rawToken,
		serverName, mcpURL, rawToken,
		serverName, mcpURL, rawToken,
	)

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "🗑 Hide token", CallbackData: callbackHideToken}},
		},
	}
	return i.api.SendMessageWithKeyboard(ctx, chatID, reply, kb)
}

func (i *Ingestor) cmdSetPolicy(ctx context.Context, chatID, userID int64, args []string) error {
	if !i.cfg.IsAdmin(userID) {
		return i.api.SendMessage(ctx, chatID, "The /set_policy command is restricted to administrators (ADMIN_TG_IDS).")
	}
	if len(args) != 2 {
		return i.api.SendMessage(ctx, chatID, "Usage: /set_policy <workspace_chat_id> <full_history|since_join|default>")
	}

	wsChatID := strings.TrimSpace(args[0])
	rawPolicy := strings.ToLower(strings.TrimSpace(args[1]))

	var policyPtr *config.HistoryPolicy
	switch rawPolicy {
	case "default", "reset":
		policyPtr = nil
	case string(config.HistoryPolicyFull), string(config.HistoryPolicySinceJoin):
		p := config.HistoryPolicy(rawPolicy)
		policyPtr = &p
	default:
		return i.api.SendMessage(ctx, chatID, "Invalid policy. Allowed values: full_history, since_join, or default.")
	}

	err := i.store.SetChatHistoryPolicyOverride(ctx, wsChatID, policyPtr)
	if errors.Is(err, storage.ErrNotFound) {
		return i.api.SendMessage(ctx, chatID, fmt.Sprintf("Chat %s not found. Check active ws= IDs via /status.", wsChatID))
	}
	if err != nil {
		return fmt.Errorf("set chat history policy: %w", err)
	}

	if policyPtr == nil {
		return i.api.SendMessage(ctx, chatID,
			fmt.Sprintf("Cleared override for chat %s — now using global policy (%s).", wsChatID, i.cfg.DefaultHistoryPolicy),
		)
	}
	return i.api.SendMessage(ctx, chatID,
		fmt.Sprintf("Updated history policy for chat %s to: %s.", wsChatID, *policyPtr),
	)
}

func (i *Ingestor) cmdStatus(ctx context.Context, chatID, userID int64) error {
	if !i.cfg.IsAdmin(userID) {
		return i.api.SendMessage(ctx, chatID, "The /status command is restricted to administrators (ADMIN_TG_IDS).")
	}

	chats, err := i.store.ListAllChats(ctx)
	if err != nil {
		return fmt.Errorf("list chats for /status: %w", err)
	}

	var b strings.Builder
	privacyState := "OK (Privacy Mode disabled)"
	if !i.canReadAll {
		privacyState = "WARNING (Privacy Mode ENABLED — disable via @BotFather /setprivacy)"
	}

	retentionLabel := "unlimited (disabled)"
	if i.cfg.RetentionDays > 0 {
		retentionLabel = fmt.Sprintf("%d days", i.cfg.RetentionDays)
	}
	oauthLabel := "off (manual /auth)"
	if i.cfg.OAuthEnabled {
		oauthLabel = fmt.Sprintf("enabled (%s)", i.cfg.PublicBaseURL)
	}
	digestLabel := "off (DIGEST_MODE=off)"
	if i.cfg.DigestMode == config.DigestModeOpenAICompatible {
		digestLabel = fmt.Sprintf("enabled (%s, %02d:00 %s)", i.cfg.DigestModel, i.cfg.DigestScheduleHour, i.cfg.DigestTimezone)
	}
	embLabel := "off (FTS5 only)"
	if i.cfg.EmbeddingMode == config.EmbeddingModeOpenAICompatible {
		embLabel = fmt.Sprintf("hybrid FTS5 + Vector (%s)", i.cfg.EmbeddingModel)
	}
	whisperLabel := "off (WHISPER_MODE=off)"
	if i.cfg.WhisperMode == config.WhisperModeOpenAICompatible {
		whisperLabel = fmt.Sprintf("enabled (%s)", i.cfg.WhisperModel)
	}

	fmt.Fprintf(&b, "mcpgram status\n")
	fmt.Fprintf(&b, "• Bot: @%s (id=%d)\n", i.botUsername, i.botID)
	fmt.Fprintf(&b, "• Group access: %s\n", privacyState)
	fmt.Fprintf(&b, "• MCP OAuth 2.1: %s\n", oauthLabel)
	fmt.Fprintf(&b, "• Default history policy: %s\n", i.cfg.DefaultHistoryPolicy)
	fmt.Fprintf(&b, "• Retention: %s | Ignore tag: %s\n", retentionLabel, i.cfg.IgnoreTag)
	fmt.Fprintf(&b, "• Nightly digests: %s\n", digestLabel)
	fmt.Fprintf(&b, "• Search: %s\n", embLabel)
	fmt.Fprintf(&b, "• Whisper: %s\n", whisperLabel)
	fmt.Fprintf(&b, "• Tracked chats: %d\n", len(chats))

	for _, c := range chats {
		adminMark := "NO ADMIN"
		if c.BotIsAdmin {
			adminMark = "admin"
		}
		policy := string(i.cfg.DefaultHistoryPolicy)
		if c.HistoryPolicyOverride != nil {
			policy = string(*c.HistoryPolicyOverride) + " (override)"
		}
		fmt.Fprintf(&b, "  - %s [%s, %s, policy=%s, ws=%s]\n",
			c.Title, c.ChatType, adminMark, policy, c.WorkspaceChatID)
	}

	return i.api.SendMessage(ctx, chatID, b.String())
}

func (i *Ingestor) cmdRevokeUser(ctx context.Context, chatID, userID int64, args []string) error {
	if !i.cfg.IsAdmin(userID) {
		return i.api.SendMessage(ctx, chatID, "The /revoke_user command is restricted to administrators (ADMIN_TG_IDS).")
	}
	if len(args) != 1 {
		return i.api.SendMessage(ctx, chatID, "Usage: /revoke_user <telegram_user_id>")
	}

	targetID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || targetID <= 0 {
		return i.api.SendMessage(ctx, chatID, "Invalid telegram_user_id. Provide a positive numeric Telegram ID.")
	}

	count, err := i.store.RevokeUserTokens(ctx, targetID)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("revoke user tokens: %w", err)
	}

	return i.api.SendMessage(ctx, chatID,
		fmt.Sprintf("Revoked all access & refresh tokens for user %d (revoked count: %d).", targetID, count),
	)
}

func (i *Ingestor) cmdDigest(ctx context.Context, chatID, userID int64, args []string) error {
	if !i.cfg.IsAdmin(userID) {
		return i.api.SendMessage(ctx, chatID, "The /digest command is restricted to administrators (ADMIN_TG_IDS).")
	}
	if i.digestRunner == nil || !i.digestRunner.Enabled() {
		return i.api.SendMessage(ctx, chatID,
			"Nightly digests are disabled. Set DIGEST_MODE=openai_compatible (plus DIGEST_BASE_URL and DIGEST_MODEL) in .env.",
		)
	}

	var generated, skipped, failed int
	var err error
	var label string

	if len(args) == 0 {
		label = fmt.Sprintf("for the last %d days", i.cfg.DigestLookbackDays)
		generated, skipped, failed, err = i.digestRunner.RunPending(ctx)
	} else {
		targetDate := strings.ToLower(strings.TrimSpace(args[0]))
		if targetDate == "today" {
			targetDate = i.digestRunner.TodayDate()
		}
		label = "for " + targetDate
		generated, skipped, failed, err = i.digestRunner.RunForDate(ctx, targetDate, true)
	}
	if err != nil {
		return i.api.SendMessage(ctx, chatID, fmt.Sprintf("Digest generation failed (%s): %v", label, err))
	}

	return i.api.SendMessage(ctx, chatID,
		fmt.Sprintf("Digests %s ready: generated=%d, skipped=%d, failed=%d.", label, generated, skipped, failed),
	)
}

func (i *Ingestor) handlePrivateDocument(ctx context.Context, msg *Message) error {
	if msg.From == nil || msg.Document == nil {
		return nil
	}
	userID := msg.From.ID
	if !i.cfg.IsAdmin(userID) {
		return i.api.SendMessage(ctx, msg.Chat.ID, "Importing chat export files (result.json / messages.html) is restricted to administrators (ADMIN_TG_IDS).")
	}

	doc := msg.Document
	lowerName := strings.ToLower(strings.TrimSpace(doc.FileName))
	lowerMime := strings.ToLower(strings.TrimSpace(doc.MimeType))
	isJSON := strings.HasSuffix(lowerName, ".json") || lowerMime == "application/json"
	isHTML := strings.HasSuffix(lowerName, ".html") || strings.HasSuffix(lowerName, ".htm") || lowerMime == "text/html"
	if !isJSON && !isHTML {
		return i.api.SendMessage(ctx, msg.Chat.ID, "Expected a Telegram chat export file: JSON (result.json) or HTML (messages.html).")
	}
	if doc.FileSize > maxBotImportFileBytes {
		return i.api.SendMessage(ctx, msg.Chat.ID,
			"File exceeds Telegram Bot API limit (20 MiB). Export a smaller date range or use CLI: mcpgram import --file result.json (or messages.html)",
		)
	}

	var chatIDOverride int64
	if capText := strings.TrimSpace(msg.Caption); capText != "" {
		fields := strings.Fields(capText)
		last := fields[len(fields)-1]
		if parsed, err := strconv.ParseInt(last, 10, 64); err == nil && parsed != 0 {
			chatIDOverride = parsed
		}
	}

	fileInfo, err := i.api.GetFile(ctx, doc.FileID)
	if err != nil {
		return i.api.SendMessage(ctx, msg.Chat.ID, fmt.Sprintf("Failed to fetch file metadata from Telegram: %v", err))
	}
	data, err := i.api.DownloadFile(ctx, fileInfo.FilePath, maxBotImportFileBytes)
	if err != nil {
		return i.api.SendMessage(ctx, msg.Chat.ID, fmt.Sprintf("Failed to download file: %v", err))
	}

	stats, err := importer.ImportDesktopReader(ctx, i.store, bytes.NewReader(data), importer.Options{
		TelegramChatIDOverride: chatIDOverride,
		IgnoreTag:              i.cfg.IgnoreTag,
		RetentionDays:          i.cfg.RetentionDays,
		Timezone:               i.cfg.DigestTimezone,
		Now:                    i.nowFunc().UTC(),
	})
	if err != nil {
		return i.api.SendMessage(ctx, msg.Chat.ID, fmt.Sprintf("Failed to import %s: %v", doc.FileName, err))
	}

	reply := fmt.Sprintf(
		"Import of %s complete (deduplicated):\n"+
			"• Chat: %s (tg_id=%d, ws=%s)\n"+
			"• Imported messages: %d\n"+
			"• Ignored (%s): %d\n"+
			"• Skipped (older than retention): %d\n"+
			"• Service/empty skipped: %d\n\n"+
			"FTS5 search is updated immediately. Semantic indexing will pick up new messages automatically.",
		doc.FileName, stats.ChatTitle, stats.TelegramChatID, stats.WorkspaceChatID,
		stats.Imported, i.cfg.IgnoreTag, stats.IgnoredOffrec,
		stats.SkippedOld, stats.SkippedNonMsg,
	)
	return i.api.SendMessage(ctx, msg.Chat.ID, reply)
}
