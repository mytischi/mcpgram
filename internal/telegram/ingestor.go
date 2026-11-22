package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/mytischi/mcpgram/internal/config"
	"github.com/mytischi/mcpgram/internal/storage"
)

// AllowedUpdates explicitly requests chat_member and my_chat_member events
// (Telegram omits chat_member unless listed explicitly).
var AllowedUpdates = []string{
	"message",
	"edited_message",
	"chat_member",
	"my_chat_member",
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

// Ingestor polls Telegram updates, stores group messages/topics/ACL, and handles DM bot commands.
type Ingestor struct {
	api          API
	store        *storage.Store
	cfg          config.Config
	access       AccessChecker
	digestRunner DigestTrigger
	logger       *slog.Logger
	pollTimeout  int
	botID        int64
	botUsername  string
	canReadAll   bool
	nowFunc      func() time.Time
}

// SetDigestRunner attaches an optional DigestTrigger for the admin /digest command.
func (i *Ingestor) SetDigestRunner(runner DigestTrigger) {
	i.digestRunner = runner
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

// Bootstrap runs getMe diagnostics, persists bot metadata, and registers private bot commands.
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
		{Command: "auth", Description: "Получить персональный Bearer-токен для MCP-клиента"},
		{Command: "status", Description: "[Admin] Проверить состояние бота и отслеживаемых чатов"},
		{Command: "digest", Description: "[Admin] Запустить генерацию дневных дайджестов"},
		{Command: "set_policy", Description: "[Admin] Политика истории для чата (full_history|since_join|default)"},
		{Command: "revoke_user", Description: "[Admin] Отозвать токены сотрудника по Telegram ID"},
	}
	if err := i.api.SetMyCommands(ctx, commands); err != nil {
		i.logger.Warn("failed to set bot commands menu", "error", err)
	}

	botRec, err := i.store.GetBot(ctx, me.ID)
	if err != nil {
		return 0, err
	}
	return botRec.UpdateOffset, nil
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
		return i.handlePrivateCommand(ctx, msg)
	}

	if msg.Chat.Type != "group" && msg.Chat.Type != "supergroup" {
		return nil
	}

	// Handle group -> supergroup chat ID migration before persisting anything.
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

	// Track Forum Topic creations and renames.
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
		// Pure service message (e.g., pinned message, topic created) — nothing to store in messages.
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
	// Strip optional @bot_username suffix (e.g. /auth@company_bot).
	if idx := strings.Index(cmd, "@"); idx != -1 {
		cmd = cmd[:idx]
	}

	switch cmd {
	case "/start", "/help":
		reply := fmt.Sprintf(
			"Привет! Я корпоративный MCP-бот (ваш Telegram ID: %d).\n\n"+
				"Доступные команды:\n"+
				"• /auth — получить персональный Bearer-токен для MCP-клиента\n"+
				"• /status — диагностика сервера (только для администраторов)\n"+
				"• /digest [YYYY-MM-DD|today] — запустить генерацию дайджестов (только для администраторов)\n"+
				"• /set_policy <workspace_chat_id> <full_history|since_join|default> — политика истории для чата (только для администраторов)\n"+
				"• /revoke_user <telegram_user_id> — мгновенный отзыв токенов (только для администраторов)",
			userID,
		)
		return i.api.SendMessage(ctx, msg.Chat.ID, reply)

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

func (i *Ingestor) cmdAuth(ctx context.Context, chatID, userID int64) error {
	allowed := i.cfg.IsAdmin(userID)
	if !allowed && i.access != nil {
		ok, err := i.access.HasAnyTrackedChatAccess(ctx, userID)
		if err != nil {
			i.logger.Error("auth membership check failed", "user_id", userID, "error", err)
			return i.api.SendMessage(ctx, chatID, "Не удалось проверить ваш доступ к рабочим чатам. Попробуйте позже.")
		}
		allowed = ok
	}

	if !allowed {
		return i.api.SendMessage(ctx, chatID,
			fmt.Sprintf("Доступ отклонён (Telegram ID: %d). Вы не состоите ни в одном отслеживаемом рабочем чате компании.", userID),
		)
	}

	ttl := time.Duration(i.cfg.TokenTTLDays) * 24 * time.Hour
	rawToken, rec, err := i.store.IssueAuthToken(ctx, userID, ttl)
	if err != nil {
		return fmt.Errorf("issue auth token: %w", err)
	}

	validity := "бессрочно, до отзыва"
	if rec.ExpiresAt > 0 {
		validity = "действует до " + time.Unix(rec.ExpiresAt, 0).UTC().Format("2006-01-02")
	}
	reply := fmt.Sprintf(
		"Ваш Telegram ID: %d\n"+
			"Персональный Bearer-токен (%s, предыдущий токен аннулирован):\n\n"+
			"%s\n\n"+
			"Добавьте его в конфиг MCP-клиента (headers.Authorization: \"Bearer %s\").\n"+
			"Важно: никогда не вставляйте этот токен в обычный чат с ИИ.",
		userID, validity, rawToken, rawToken,
	)
	return i.api.SendMessage(ctx, chatID, reply)
}

func (i *Ingestor) cmdSetPolicy(ctx context.Context, chatID, userID int64, args []string) error {
	if !i.cfg.IsAdmin(userID) {
		return i.api.SendMessage(ctx, chatID, "Команда /set_policy доступна только администраторам (ADMIN_TG_IDS).")
	}
	if len(args) != 2 {
		return i.api.SendMessage(ctx, chatID, "Использование: /set_policy <workspace_chat_id> <full_history|since_join|default>")
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
		return i.api.SendMessage(ctx, chatID, "Некорректная политика. Допустимо: full_history, since_join или default.")
	}

	err := i.store.SetChatHistoryPolicyOverride(ctx, wsChatID, policyPtr)
	if errors.Is(err, storage.ErrNotFound) {
		return i.api.SendMessage(ctx, chatID, fmt.Sprintf("Чат %s не найден. Посмотрите актуальный ws= в /status.", wsChatID))
	}
	if err != nil {
		return fmt.Errorf("set chat history policy: %w", err)
	}

	if policyPtr == nil {
		return i.api.SendMessage(ctx, chatID,
			fmt.Sprintf("Для чата %s сброшено переопределение — используется глобальная политика (%s).", wsChatID, i.cfg.DefaultHistoryPolicy),
		)
	}
	return i.api.SendMessage(ctx, chatID,
		fmt.Sprintf("Для чата %s установлена политика истории: %s.", wsChatID, *policyPtr),
	)
}

func (i *Ingestor) cmdStatus(ctx context.Context, chatID, userID int64) error {
	if !i.cfg.IsAdmin(userID) {
		return i.api.SendMessage(ctx, chatID, "Команда /status доступна только администраторам (ADMIN_TG_IDS).")
	}

	chats, err := i.store.ListAllChats(ctx)
	if err != nil {
		return fmt.Errorf("list chats for /status: %w", err)
	}

	var b strings.Builder
	privacyState := "OK (Privacy Mode отключен)"
	if !i.canReadAll {
		privacyState = "WARNING (Privacy Mode ВКЛЮЧЕН — отключите в @BotFather через /setprivacy)"
	}

	fmt.Fprintf(&b, "mcpgram status\n")
	fmt.Fprintf(&b, "• Бот: @%s (id=%d)\n", i.botUsername, i.botID)
	fmt.Fprintf(&b, "• Чтение групп: %s\n", privacyState)
	fmt.Fprintf(&b, "• Политика истории по умолчанию: %s\n", i.cfg.DefaultHistoryPolicy)
	retentionLabel := "бессрочно (отключено)"
	if i.cfg.RetentionDays > 0 {
		retentionLabel = fmt.Sprintf("%d дн.", i.cfg.RetentionDays)
	}
	digestLabel := "отключены (DIGEST_MODE=off)"
	if i.cfg.DigestMode == config.DigestModeOpenAICompatible {
		digestLabel = fmt.Sprintf("включены (%s, %02d:00 %s)", i.cfg.DigestModel, i.cfg.DigestScheduleHour, i.cfg.DigestTimezone)
	}
	embLabel := "выкл (только FTS5)"
	if i.cfg.EmbeddingMode == config.EmbeddingModeOpenAICompatible {
		embLabel = fmt.Sprintf("гибридный FTS5 + Vector (%s)", i.cfg.EmbeddingModel)
	}
	whisperLabel := "выкл (WHISPER_MODE=off)"
	if i.cfg.WhisperMode == config.WhisperModeOpenAICompatible {
		whisperLabel = fmt.Sprintf("вкл (%s)", i.cfg.WhisperModel)
	}
	fmt.Fprintf(&b, "• Retention: %s | Ignore tag: %s\n", retentionLabel, i.cfg.IgnoreTag)
	fmt.Fprintf(&b, "• Ночные дайджесты: %s\n", digestLabel)
	fmt.Fprintf(&b, "• Поиск: %s\n", embLabel)
	fmt.Fprintf(&b, "• Whisper (голосовые): %s\n", whisperLabel)
	fmt.Fprintf(&b, "• Отслеживаемых чатов: %d\n", len(chats))

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
		return i.api.SendMessage(ctx, chatID, "Команда /revoke_user доступна только администраторам (ADMIN_TG_IDS).")
	}
	if len(args) != 1 {
		return i.api.SendMessage(ctx, chatID, "Использование: /revoke_user <telegram_user_id>")
	}

	targetID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || targetID <= 0 {
		return i.api.SendMessage(ctx, chatID, "Некорректный telegram_user_id. Укажите числовой ID пользователя.")
	}

	count, err := i.store.RevokeUserTokens(ctx, targetID)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("revoke user tokens: %w", err)
	}

	return i.api.SendMessage(ctx, chatID,
		fmt.Sprintf("Токены пользователя %d отозваны (активных аннулировано: %d).", targetID, count),
	)
}

func (i *Ingestor) cmdDigest(ctx context.Context, chatID, userID int64, args []string) error {
	if !i.cfg.IsAdmin(userID) {
		return i.api.SendMessage(ctx, chatID, "Команда /digest доступна только администраторам (ADMIN_TG_IDS).")
	}
	if i.digestRunner == nil || !i.digestRunner.Enabled() {
		return i.api.SendMessage(ctx, chatID,
			"Ночные дайджесты отключены. Включите DIGEST_MODE=openai_compatible (и DIGEST_BASE_URL + DIGEST_MODEL) в .env.",
		)
	}

	var generated, skipped, failed int
	var err error
	var label string

	if len(args) == 0 {
		label = fmt.Sprintf("за последние %d дн.", i.cfg.DigestLookbackDays)
		generated, skipped, failed, err = i.digestRunner.RunPending(ctx)
	} else {
		targetDate := strings.ToLower(strings.TrimSpace(args[0]))
		if targetDate == "today" {
			targetDate = i.digestRunner.TodayDate()
		}
		label = "за " + targetDate
		generated, skipped, failed, err = i.digestRunner.RunForDate(ctx, targetDate, true)
	}
	if err != nil {
		return i.api.SendMessage(ctx, chatID, fmt.Sprintf("Ошибка генерации дайджестов (%s): %v", label, err))
	}

	return i.api.SendMessage(ctx, chatID,
		fmt.Sprintf("Дайджесты %s готовы: создано/обновлено=%d, пропущено=%d, ошибок=%d.", label, generated, skipped, failed),
	)
}
