package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mytischi/mcpgram/internal/config"
)

var (
	// ErrNotFound is returned when a requested entity does not exist.
	ErrNotFound = errors.New("not found")
	// ErrInvalidToken is returned when a Bearer token is unknown, revoked, or expired.
	ErrInvalidToken = errors.New("invalid or expired token")
)

// UpsertBot inserts or updates bot metadata while preserving update_offset.
func (s *Store) UpsertBot(ctx context.Context, bot Bot) error {
	now := time.Now().UTC().Unix()
	if bot.CreatedAt == 0 {
		bot.CreatedAt = now
	}
	bot.UpdatedAt = now

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO bots (
			bot_id, username, first_name, can_read_all_group_messages,
			update_offset, last_poll_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(bot_id) DO UPDATE SET
			username = excluded.username,
			first_name = excluded.first_name,
			can_read_all_group_messages = excluded.can_read_all_group_messages,
			updated_at = excluded.updated_at
	`,
		bot.BotID,
		bot.Username,
		bot.FirstName,
		boolToInt(bot.CanReadAllGroupMessages),
		bot.UpdateOffset,
		bot.LastPollAt,
		bot.CreatedAt,
		bot.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert bot %d: %w", bot.BotID, err)
	}
	return nil
}

// GetBot fetches a bot by its Telegram numeric ID.
func (s *Store) GetBot(ctx context.Context, botID int64) (Bot, error) {
	var b Bot
	var canRead int
	var lastPoll sql.NullInt64

	err := s.db.QueryRowContext(ctx, `
		SELECT bot_id, username, first_name, can_read_all_group_messages,
		       update_offset, last_poll_at, created_at, updated_at
		FROM bots WHERE bot_id = ?
	`, botID).Scan(
		&b.BotID, &b.Username, &b.FirstName, &canRead,
		&b.UpdateOffset, &lastPoll, &b.CreatedAt, &b.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Bot{}, ErrNotFound
	}
	if err != nil {
		return Bot{}, fmt.Errorf("get bot %d: %w", botID, err)
	}
	b.CanReadAllGroupMessages = canRead != 0
	if lastPoll.Valid {
		v := lastPoll.Int64
		b.LastPollAt = &v
	}
	return b, nil
}

// UpdateBotOffset persists the latest Telegram getUpdates offset and poll timestamp.
func (s *Store) UpdateBotOffset(ctx context.Context, botID, offset int64, polledAt int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE bots
		SET update_offset = CASE WHEN ? > update_offset THEN ? ELSE update_offset END,
		    last_poll_at = ?,
		    updated_at = ?
		WHERE bot_id = ?
	`, offset, offset, polledAt, polledAt, botID)
	if err != nil {
		return fmt.Errorf("update bot offset %d: %w", botID, err)
	}
	return nil
}

// LatestBotPollTime returns the most recent polling timestamp across registered bots,
// or nil if no poll has completed yet.
func (s *Store) LatestBotPollTime(ctx context.Context) (*int64, error) {
	var ts sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(last_poll_at) FROM bots`).Scan(&ts); err != nil {
		return nil, fmt.Errorf("query latest bot poll time: %w", err)
	}
	if !ts.Valid {
		return nil, nil
	}
	v := ts.Int64
	return &v, nil
}

// ResolveWorkspaceChatID resolves a current or migrated Telegram chat ID to its internal workspace_chat_id.
func (s *Store) ResolveWorkspaceChatID(ctx context.Context, telegramChatID int64) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT workspace_chat_id FROM chats WHERE telegram_chat_id = ?`,
		telegramChatID,
	).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("lookup chat %d: %w", telegramChatID, err)
	}

	err = s.db.QueryRowContext(ctx,
		`SELECT workspace_chat_id FROM chat_id_aliases WHERE telegram_chat_id = ?`,
		telegramChatID,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lookup chat alias %d: %w", telegramChatID, err)
	}
	return id, nil
}

// UpsertChat creates or updates a tracked chat, returning its stable workspace_chat_id.
func (s *Store) UpsertChat(ctx context.Context, chat Chat) (Chat, error) {
	now := time.Now().UTC().Unix()

	existingID, err := s.ResolveWorkspaceChatID(ctx, chat.TelegramChatID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Chat{}, err
	}

	if existingID == "" {
		if chat.WorkspaceChatID == "" {
			chat.WorkspaceChatID = uuid.NewString()
		}
		if chat.CreatedAt == 0 {
			chat.CreatedAt = now
		}
		chat.UpdatedAt = now

		var policyOverride any
		if chat.HistoryPolicyOverride != nil {
			policyOverride = string(*chat.HistoryPolicyOverride)
		}

		_, err := s.db.ExecContext(ctx, `
			INSERT INTO chats (
				workspace_chat_id, telegram_chat_id, assigned_bot_id,
				title, chat_type, is_forum, bot_is_admin, bot_status,
				history_policy_override, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			chat.WorkspaceChatID,
			chat.TelegramChatID,
			chat.AssignedBotID,
			chat.Title,
			chat.ChatType,
			boolToInt(chat.IsForum),
			boolToInt(chat.BotIsAdmin),
			chat.BotStatus,
			policyOverride,
			chat.CreatedAt,
			chat.UpdatedAt,
		)
		if err != nil {
			return Chat{}, fmt.Errorf("insert chat %d: %w", chat.TelegramChatID, err)
		}
		return s.GetChatByWorkspaceID(ctx, chat.WorkspaceChatID)
	}

	_, err = s.db.ExecContext(ctx, `
		UPDATE chats
		SET assigned_bot_id = COALESCE(?, assigned_bot_id),
		    title = CASE WHEN ? != '' THEN ? ELSE title END,
		    chat_type = CASE WHEN ? != '' THEN ? ELSE chat_type END,
		    is_forum = CASE WHEN ? = 1 THEN 1 ELSE is_forum END,
		    bot_is_admin = CASE WHEN ? != '' THEN ? ELSE bot_is_admin END,
		    bot_status = CASE WHEN ? != '' THEN ? ELSE bot_status END,
		    updated_at = ?
		WHERE workspace_chat_id = ?
	`,
		chat.AssignedBotID,
		chat.Title, chat.Title,
		chat.ChatType, chat.ChatType,
		boolToInt(chat.IsForum),
		chat.BotStatus, boolToInt(chat.BotIsAdmin),
		chat.BotStatus, chat.BotStatus,
		now,
		existingID,
	)
	if err != nil {
		return Chat{}, fmt.Errorf("update chat %s: %w", existingID, err)
	}

	return s.GetChatByWorkspaceID(ctx, existingID)
}

// GetChatByWorkspaceID loads a chat by its internal UUID.
func (s *Store) GetChatByWorkspaceID(ctx context.Context, workspaceChatID string) (Chat, error) {
	var c Chat
	var assignedBot sql.NullInt64
	var isForum, botIsAdmin int
	var policyOverride sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT workspace_chat_id, telegram_chat_id, assigned_bot_id,
		       title, chat_type, is_forum, bot_is_admin, bot_status,
		       history_policy_override, created_at, updated_at
		FROM chats
		WHERE workspace_chat_id = ?
	`, workspaceChatID).Scan(
		&c.WorkspaceChatID, &c.TelegramChatID, &assignedBot,
		&c.Title, &c.ChatType, &isForum, &botIsAdmin, &c.BotStatus,
		&policyOverride, &c.CreatedAt, &c.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Chat{}, ErrNotFound
	}
	if err != nil {
		return Chat{}, fmt.Errorf("get chat %s: %w", workspaceChatID, err)
	}
	if assignedBot.Valid {
		v := assignedBot.Int64
		c.AssignedBotID = &v
	}
	c.IsForum = isForum != 0
	c.BotIsAdmin = botIsAdmin != 0
	if policyOverride.Valid {
		p := config.HistoryPolicy(policyOverride.String)
		c.HistoryPolicyOverride = &p
	}
	return c, nil
}

// SetChatHistoryPolicyOverride sets or clears (when nil) a per-chat history policy override.
func (s *Store) SetChatHistoryPolicyOverride(ctx context.Context, workspaceChatID string, policy *config.HistoryPolicy) error {
	var val any
	if policy != nil {
		if !policy.Valid() {
			return fmt.Errorf("invalid history policy %q", *policy)
		}
		val = string(*policy)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE chats SET history_policy_override = ?, updated_at = ?
		WHERE workspace_chat_id = ?
	`, val, time.Now().UTC().Unix(), workspaceChatID)
	if err != nil {
		return fmt.Errorf("set history policy override: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// MigrateChat handles Telegram group -> supergroup ID changes without losing message history,
// topics, or ACL entries. It also safely merges if Telegram delivered updates for the new chat ID
// before the migration service message arrived.
func (s *Store) MigrateChat(ctx context.Context, oldTelegramChatID, newTelegramChatID int64) (string, error) {
	if oldTelegramChatID == newTelegramChatID {
		return s.ResolveWorkspaceChatID(ctx, oldTelegramChatID)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin migrate chat: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Unix()

	lookupTx := func(tgID int64) (string, error) {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT workspace_chat_id FROM chats WHERE telegram_chat_id = ?`, tgID).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		err = tx.QueryRowContext(ctx, `SELECT workspace_chat_id FROM chat_id_aliases WHERE telegram_chat_id = ?`, tgID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return id, err
	}

	oldWS, err := lookupTx(oldTelegramChatID)
	if err != nil {
		return "", fmt.Errorf("lookup old chat %d: %w", oldTelegramChatID, err)
	}
	newWS, err := lookupTx(newTelegramChatID)
	if err != nil {
		return "", fmt.Errorf("lookup new chat %d: %w", newTelegramChatID, err)
	}

	var targetWS string
	switch {
	case oldWS == "" && newWS == "":
		targetWS = uuid.NewString()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO chats (workspace_chat_id, telegram_chat_id, chat_type, created_at, updated_at)
			VALUES (?, ?, 'supergroup', ?, ?)
		`, targetWS, newTelegramChatID, now, now); err != nil {
			return "", fmt.Errorf("insert migrated chat: %w", err)
		}
	case oldWS == "" && newWS != "":
		targetWS = newWS
	case oldWS != "" && newWS == "":
		targetWS = oldWS
		if _, err := tx.ExecContext(ctx, `
			UPDATE chats
			SET telegram_chat_id = ?, chat_type = 'supergroup', updated_at = ?
			WHERE workspace_chat_id = ?
		`, newTelegramChatID, now, targetWS); err != nil {
			return "", fmt.Errorf("rebind migrated chat id: %w", err)
		}
	case oldWS == newWS:
		targetWS = oldWS
	default:
		// Both exist as separate rows (out-of-order delivery): merge newWS into oldWS.
		targetWS = oldWS

		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO topics (workspace_chat_id, message_thread_id, title, created_at, updated_at)
			SELECT ?, message_thread_id, title, created_at, updated_at
			FROM topics WHERE workspace_chat_id = ?
		`, targetWS, newWS); err != nil {
			return "", fmt.Errorf("merge topics: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO messages (
				workspace_chat_id, telegram_message_id, sender_tg_id, sender_name,
				sender_username, text, reply_to_message_id, message_thread_id,
				attachment_meta, created_at, edited_at
			)
			SELECT
				?, telegram_message_id, sender_tg_id, sender_name,
				sender_username, text, reply_to_message_id, message_thread_id,
				attachment_meta, created_at, edited_at
			FROM messages WHERE workspace_chat_id = ?
		`, targetWS, newWS); err != nil {
			return "", fmt.Errorf("merge messages: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO chat_acl (workspace_chat_id, telegram_user_id, status, is_member, joined_at, checked_at)
			SELECT ?, telegram_user_id, status, is_member, joined_at, checked_at
			FROM chat_acl WHERE workspace_chat_id = ?
			ON CONFLICT(workspace_chat_id, telegram_user_id) DO UPDATE SET
				status = CASE WHEN excluded.checked_at >= chat_acl.checked_at THEN excluded.status ELSE chat_acl.status END,
				is_member = CASE WHEN excluded.checked_at >= chat_acl.checked_at THEN excluded.is_member ELSE chat_acl.is_member END,
				joined_at = COALESCE(chat_acl.joined_at, excluded.joined_at),
				checked_at = MAX(chat_acl.checked_at, excluded.checked_at)
		`, targetWS, newWS); err != nil {
			return "", fmt.Errorf("merge chat_acl: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO daily_digests (
				workspace_chat_id, digest_date, period_start, period_end,
				message_count, last_message_id, summary,
				key_decisions, action_items, topics,
				model, created_at, updated_at
			)
			SELECT
				?, digest_date, period_start, period_end,
				message_count, last_message_id, summary,
				key_decisions, action_items, topics,
				model, created_at, updated_at
			FROM daily_digests WHERE workspace_chat_id = ?
		`, targetWS, newWS); err != nil {
			return "", fmt.Errorf("merge daily_digests: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO message_chunks (
				workspace_chat_id, message_thread_id, first_message_id, last_message_id,
				min_created_at, max_created_at, chunk_text, content_hash,
				model, dimensions, embedding, created_at, updated_at
			)
			SELECT
				?, message_thread_id, first_message_id, last_message_id,
				min_created_at, max_created_at, chunk_text, content_hash,
				model, dimensions, embedding, created_at, updated_at
			FROM message_chunks WHERE workspace_chat_id = ?
		`, targetWS, newWS); err != nil {
			return "", fmt.Errorf("merge message_chunks: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE chat_id_aliases SET workspace_chat_id = ? WHERE workspace_chat_id = ?
		`, targetWS, newWS); err != nil {
			return "", fmt.Errorf("repoint aliases: %w", err)
		}

		// Delete duplicate chat row first to free the UNIQUE telegram_chat_id constraint.
		if _, err := tx.ExecContext(ctx, `DELETE FROM chats WHERE workspace_chat_id = ?`, newWS); err != nil {
			return "", fmt.Errorf("delete merged duplicate chat: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE chats
			SET telegram_chat_id = ?, chat_type = 'supergroup', updated_at = ?
			WHERE workspace_chat_id = ?
		`, newTelegramChatID, now, targetWS); err != nil {
			return "", fmt.Errorf("update merged chat telegram_chat_id: %w", err)
		}
	}

	for _, tgID := range []int64{oldTelegramChatID, newTelegramChatID} {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO chat_id_aliases (telegram_chat_id, workspace_chat_id, migrated_at)
			VALUES (?, ?, ?)
			ON CONFLICT(telegram_chat_id) DO UPDATE SET
				workspace_chat_id = excluded.workspace_chat_id,
				migrated_at = excluded.migrated_at
		`, tgID, targetWS, now); err != nil {
			return "", fmt.Errorf("record chat alias %d: %w", tgID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit migrate chat: %w", err)
	}
	return targetWS, nil
}

// UpsertTopic saves or updates a Forum Topic title.
func (s *Store) UpsertTopic(ctx context.Context, topic Topic) error {
	now := time.Now().UTC().Unix()
	if topic.CreatedAt == 0 {
		topic.CreatedAt = now
	}
	topic.UpdatedAt = now

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO topics (workspace_chat_id, message_thread_id, title, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(workspace_chat_id, message_thread_id) DO UPDATE SET
			title = excluded.title,
			updated_at = excluded.updated_at
	`, topic.WorkspaceChatID, topic.MessageThreadID, topic.Title, topic.CreatedAt, topic.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert topic %s/%d: %w", topic.WorkspaceChatID, topic.MessageThreadID, err)
	}
	return nil
}

// UpsertMessage inserts or updates a message. If the message text contains ignoreTag
// (e.g. #offrecord), it is not persisted; if an existing message was edited to add ignoreTag,
// it is removed from the database. Returns ignored=true in that case.
func (s *Store) UpsertMessage(ctx context.Context, msg Message, ignoreTag string) (ignored bool, err error) {
	if ShouldIgnoreMessage(msg.Text, ignoreTag) {
		_, err := s.db.ExecContext(ctx, `
			DELETE FROM messages
			WHERE workspace_chat_id = ? AND telegram_message_id = ?
		`, msg.WorkspaceChatID, msg.TelegramMessageID)
		if err != nil {
			return true, fmt.Errorf("delete offrecord message: %w", err)
		}
		return true, nil
	}

	var attachJSON sql.NullString
	if msg.AttachmentMeta != nil {
		raw, err := json.Marshal(msg.AttachmentMeta)
		if err != nil {
			return false, fmt.Errorf("marshal attachment_meta: %w", err)
		}
		attachJSON = sql.NullString{String: string(raw), Valid: true}
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO messages (
			workspace_chat_id, telegram_message_id, sender_tg_id,
			sender_name, sender_username, text,
			reply_to_message_id, message_thread_id, attachment_meta,
			created_at, edited_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(workspace_chat_id, telegram_message_id) DO UPDATE SET
			sender_name = CASE WHEN excluded.sender_name != '' THEN excluded.sender_name ELSE messages.sender_name END,
			sender_username = CASE WHEN excluded.sender_username != '' THEN excluded.sender_username ELSE messages.sender_username END,
			text = excluded.text,
			attachment_meta = COALESCE(excluded.attachment_meta, messages.attachment_meta),
			edited_at = COALESCE(excluded.edited_at, messages.edited_at)
	`,
		msg.WorkspaceChatID,
		msg.TelegramMessageID,
		msg.SenderTGID,
		msg.SenderName,
		msg.SenderUsername,
		msg.Text,
		msg.ReplyToMessageID,
		msg.MessageThreadID,
		attachJSON,
		msg.CreatedAt,
		msg.EditedAt,
	)
	if err != nil {
		return false, fmt.Errorf("upsert message %s/%d: %w", msg.WorkspaceChatID, msg.TelegramMessageID, err)
	}
	return false, nil
}

// UpsertChatACL records or updates a user's membership status in a chat.
// An existing joined_at is preserved unless a newer join timestamp is explicitly provided.
func (s *Store) UpsertChatACL(ctx context.Context, acl ChatACL) error {
	if acl.CheckedAt == 0 {
		acl.CheckedAt = time.Now().UTC().Unix()
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO chat_acl (
			workspace_chat_id, telegram_user_id, status, is_member, joined_at, checked_at
		) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(workspace_chat_id, telegram_user_id) DO UPDATE SET
			status = excluded.status,
			is_member = excluded.is_member,
			joined_at = COALESCE(excluded.joined_at, chat_acl.joined_at),
			checked_at = excluded.checked_at
	`,
		acl.WorkspaceChatID,
		acl.TelegramUserID,
		strings.ToLower(strings.TrimSpace(acl.Status)),
		boolToInt(acl.IsMember),
		acl.JoinedAt,
		acl.CheckedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert chat_acl %s/%d: %w", acl.WorkspaceChatID, acl.TelegramUserID, err)
	}
	return nil
}

// GetChatACL returns the cached ACL entry for a user in a chat.
func (s *Store) GetChatACL(ctx context.Context, workspaceChatID string, telegramUserID int64) (ChatACL, error) {
	var acl ChatACL
	var isMember int
	var joinedAt sql.NullInt64

	err := s.db.QueryRowContext(ctx, `
		SELECT workspace_chat_id, telegram_user_id, status, is_member, joined_at, checked_at
		FROM chat_acl
		WHERE workspace_chat_id = ? AND telegram_user_id = ?
	`, workspaceChatID, telegramUserID).Scan(
		&acl.WorkspaceChatID, &acl.TelegramUserID, &acl.Status,
		&isMember, &joinedAt, &acl.CheckedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatACL{}, ErrNotFound
	}
	if err != nil {
		return ChatACL{}, fmt.Errorf("get chat_acl: %w", err)
	}
	acl.IsMember = isMember != 0
	if joinedAt.Valid {
		v := joinedAt.Int64
		acl.JoinedAt = &v
	}
	return acl, nil
}

// ListAllChats returns all tracked chats (for admin diagnostics and RBAC membership probing).
func (s *Store) ListAllChats(ctx context.Context) ([]Chat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT workspace_chat_id, telegram_chat_id, assigned_bot_id,
		       title, chat_type, is_forum, bot_is_admin, bot_status,
		       history_policy_override, created_at, updated_at
		FROM chats
		ORDER BY title ASC, created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list all chats: %w", err)
	}
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		var c Chat
		var assignedBot sql.NullInt64
		var isForum, botIsAdmin int
		var policyOverride sql.NullString
		if err := rows.Scan(
			&c.WorkspaceChatID, &c.TelegramChatID, &assignedBot,
			&c.Title, &c.ChatType, &isForum, &botIsAdmin, &c.BotStatus,
			&policyOverride, &c.CreatedAt, &c.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan chat: %w", err)
		}
		if assignedBot.Valid {
			v := assignedBot.Int64
			c.AssignedBotID = &v
		}
		c.IsForum = isForum != 0
		c.BotIsAdmin = botIsAdmin != 0
		if policyOverride.Valid {
			p := config.HistoryPolicy(policyOverride.String)
			c.HistoryPolicyOverride = &p
		}
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

// ListAccessibleChats returns chats accessible to telegramUserID according to chat_acl,
// including forum topics and last message timestamp. When minACLCheckedAt > 0, stale ACL
// records older than that timestamp are excluded (fail-closed).
func (s *Store) ListAccessibleChats(ctx context.Context, telegramUserID int64, defaultPolicy config.HistoryPolicy, minACLCheckedAt ...int64) ([]ChatSummary, error) {
	if !defaultPolicy.Valid() {
		defaultPolicy = config.HistoryPolicyFull
	}
	var minChecked int64
	if len(minACLCheckedAt) > 0 {
		minChecked = minACLCheckedAt[0]
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT
			c.workspace_chat_id,
			c.title,
			c.chat_type,
			c.is_forum,
			MAX(m.created_at) AS last_message_at,
			COUNT(m.id) AS message_count
		FROM chats c
		JOIN chat_acl acl
			ON acl.workspace_chat_id = c.workspace_chat_id
			AND acl.telegram_user_id = ?
		LEFT JOIN messages m
			ON m.workspace_chat_id = c.workspace_chat_id
			AND (
				COALESCE(c.history_policy_override, ?) = 'full_history'
				OR acl.joined_at IS NULL
				OR m.created_at >= acl.joined_at
			)
		WHERE (
			acl.status IN ('creator', 'administrator', 'member')
			OR (acl.status = 'restricted' AND acl.is_member = 1)
		)
		  AND (? <= 0 OR acl.checked_at >= ?)
		GROUP BY c.workspace_chat_id, c.title, c.chat_type, c.is_forum
		ORDER BY COALESCE(MAX(m.created_at), c.updated_at) DESC, c.title ASC
	`, telegramUserID, string(defaultPolicy), minChecked, minChecked)
	if err != nil {
		return nil, fmt.Errorf("list accessible chats: %w", err)
	}
	defer rows.Close()

	var summaries []ChatSummary
	for rows.Next() {
		var cs ChatSummary
		var isForum int
		var lastMsg sql.NullInt64
		if err := rows.Scan(
			&cs.WorkspaceChatID, &cs.Title, &cs.ChatType,
			&isForum, &lastMsg, &cs.MessageCount,
		); err != nil {
			return nil, fmt.Errorf("scan chat summary: %w", err)
		}
		cs.IsForum = isForum != 0
		if lastMsg.Valid {
			v := lastMsg.Int64
			cs.LastMessageAt = &v
		}
		summaries = append(summaries, cs)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range summaries {
		topics, err := s.listTopics(ctx, summaries[i].WorkspaceChatID)
		if err != nil {
			return nil, err
		}
		summaries[i].Topics = topics
	}

	return summaries, nil
}

func (s *Store) listTopics(ctx context.Context, workspaceChatID string) ([]Topic, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT workspace_chat_id, message_thread_id, title, created_at, updated_at
		FROM topics
		WHERE workspace_chat_id = ?
		ORDER BY title ASC, message_thread_id ASC
	`, workspaceChatID)
	if err != nil {
		return nil, fmt.Errorf("list topics: %w", err)
	}
	defer rows.Close()

	var topics []Topic
	for rows.Next() {
		var t Topic
		if err := rows.Scan(&t.WorkspaceChatID, &t.MessageThreadID, &t.Title, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		topics = append(topics, t)
	}
	return topics, rows.Err()
}

// GetMessages returns chronological messages for a chat strictly filtered by RBAC and history policy.
func (s *Store) GetMessages(ctx context.Context, filter MessageFilter) ([]Message, error) {
	policy := filter.DefaultHistoryPolicy
	if !policy.Valid() {
		policy = config.HistoryPolicyFull
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	query := `
		SELECT
			m.id,
			m.workspace_chat_id,
			m.telegram_message_id,
			m.sender_tg_id,
			m.sender_name,
			m.sender_username,
			m.text,
			m.reply_to_message_id,
			m.message_thread_id,
			m.attachment_meta,
			m.created_at,
			m.edited_at
		FROM messages m
		JOIN chats c ON c.workspace_chat_id = m.workspace_chat_id
		JOIN chat_acl acl
			ON acl.workspace_chat_id = m.workspace_chat_id
			AND acl.telegram_user_id = ?
		WHERE m.workspace_chat_id = ?
		  AND (
			acl.status IN ('creator', 'administrator', 'member')
			OR (acl.status = 'restricted' AND acl.is_member = 1)
		  )
		  AND (? <= 0 OR acl.checked_at >= ?)
		  AND (
			COALESCE(c.history_policy_override, ?) = 'full_history'
			OR acl.joined_at IS NULL
			OR m.created_at >= acl.joined_at
		  )
	`
	args := []any{filter.TelegramUserID, filter.WorkspaceChatID, filter.MinACLCheckedAt, filter.MinACLCheckedAt, string(policy)}

	if filter.FromUnix != nil {
		query += ` AND m.created_at >= ?`
		args = append(args, *filter.FromUnix)
	}
	if filter.ToUnix != nil {
		query += ` AND m.created_at <= ?`
		args = append(args, *filter.ToUnix)
	}
	if filter.TopicID != nil {
		query += ` AND m.message_thread_id = ?`
		args = append(args, *filter.TopicID)
	}
	if author := strings.TrimSpace(filter.Author); author != "" {
		like := "%" + strings.ToLower(strings.TrimPrefix(author, "@")) + "%"
		query += ` AND (LOWER(m.sender_name) LIKE ? OR LOWER(m.sender_username) LIKE ?)`
		args = append(args, like, like)
	}

	query += ` ORDER BY m.created_at ASC, m.telegram_message_id ASC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get messages: %w", err)
	}
	defer rows.Close()

	return scanMessages(rows)
}

// GetThread reconstructs the full reply chain containing params.TelegramMessageID
// (walking up to the root message and down to all replies) while enforcing RBAC and history policy.
func (s *Store) GetThread(ctx context.Context, params ThreadParams) ([]Message, error) {
	policy := params.DefaultHistoryPolicy
	if !policy.Valid() {
		policy = config.HistoryPolicyFull
	}

	limit := params.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 200 {
		limit = 200
	}

	query := `
		WITH visible AS (
			SELECT
				m.id,
				m.workspace_chat_id,
				m.telegram_message_id,
				m.sender_tg_id,
				m.sender_name,
				m.sender_username,
				m.text,
				m.reply_to_message_id,
				m.message_thread_id,
				m.attachment_meta,
				m.created_at,
				m.edited_at
			FROM messages m
			JOIN chats c ON c.workspace_chat_id = m.workspace_chat_id
			JOIN chat_acl acl
				ON acl.workspace_chat_id = m.workspace_chat_id
				AND acl.telegram_user_id = ?
			WHERE m.workspace_chat_id = ?
			  AND (
				acl.status IN ('creator', 'administrator', 'member')
				OR (acl.status = 'restricted' AND acl.is_member = 1)
			  )
			  AND (? <= 0 OR acl.checked_at >= ?)
			  AND (
				COALESCE(c.history_policy_override, ?) = 'full_history'
				OR acl.joined_at IS NULL
				OR m.created_at >= acl.joined_at
			  )
		),
		ancestors(telegram_message_id, reply_to_message_id, depth) AS (
			SELECT telegram_message_id, reply_to_message_id, 0
			FROM visible
			WHERE telegram_message_id = ?
			UNION
			SELECT v.telegram_message_id, v.reply_to_message_id, a.depth + 1
			FROM visible v
			JOIN ancestors a ON v.telegram_message_id = a.reply_to_message_id
			WHERE a.depth < 100
		),
		root_msg AS (
			SELECT telegram_message_id
			FROM ancestors
			ORDER BY depth DESC
			LIMIT 1
		),
		thread(telegram_message_id, depth) AS (
			SELECT telegram_message_id, 0
			FROM root_msg
			UNION
			SELECT v.telegram_message_id, t.depth + 1
			FROM visible v
			JOIN thread t ON v.reply_to_message_id = t.telegram_message_id
			WHERE t.depth < 100
		)
		SELECT
			v.id,
			v.workspace_chat_id,
			v.telegram_message_id,
			v.sender_tg_id,
			v.sender_name,
			v.sender_username,
			v.text,
			v.reply_to_message_id,
			v.message_thread_id,
			v.attachment_meta,
			v.created_at,
			v.edited_at
		FROM visible v
		JOIN thread t ON t.telegram_message_id = v.telegram_message_id
		ORDER BY v.created_at ASC, v.telegram_message_id ASC
		LIMIT ?
	`

	rows, err := s.db.QueryContext(ctx, query,
		params.TelegramUserID,
		params.WorkspaceChatID,
		params.MinACLCheckedAt,
		params.MinACLCheckedAt,
		string(policy),
		params.TelegramMessageID,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get thread: %w", err)
	}
	defer rows.Close()

	return scanMessages(rows)
}

// IssueAuthToken revokes any active tokens for telegramUserID and issues a fresh Bearer token.
// Only the SHA-256 hash is stored in the database; rawToken is returned once to the caller.
// When ttl <= 0, the token does not expire automatically (expires_at = 0) until explicitly revoked.
func (s *Store) IssueAuthToken(ctx context.Context, telegramUserID int64, ttl time.Duration) (rawToken string, rec AuthToken, err error) {
	return s.issueAuthToken(ctx, telegramUserID, ttl, true)
}

// IssueAccessToken issues a Bearer access token without revoking the user's other active tokens
// (so OAuth 2.1 clients like Cursor and Claude Code can stay authenticated concurrently).
func (s *Store) IssueAccessToken(ctx context.Context, telegramUserID int64, ttl time.Duration) (rawToken string, rec AuthToken, err error) {
	return s.issueAuthToken(ctx, telegramUserID, ttl, false)
}

func (s *Store) issueAuthToken(ctx context.Context, telegramUserID int64, ttl time.Duration, revokePrevious bool) (rawToken string, rec AuthToken, err error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", AuthToken{}, fmt.Errorf("generate token entropy: %w", err)
	}
	rawToken = "tgmcp_" + hex.EncodeToString(buf)
	hash := hashToken(rawToken)
	prefix := rawToken[:12]

	now := time.Now().UTC()
	nowUnix := now.Unix()
	var expiresUnix int64
	if ttl > 0 {
		expiresUnix = now.Add(ttl).Unix()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", AuthToken{}, fmt.Errorf("begin issue token: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if revokePrevious {
		if _, err := tx.ExecContext(ctx, `
			UPDATE auth_tokens
			SET revoked_at = ?
			WHERE telegram_user_id = ? AND revoked_at IS NULL
		`, nowUnix, telegramUserID); err != nil {
			return "", AuthToken{}, fmt.Errorf("revoke previous tokens: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO auth_tokens (token_hash, token_prefix, telegram_user_id, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)
	`, hash, prefix, telegramUserID, nowUnix, expiresUnix); err != nil {
		return "", AuthToken{}, fmt.Errorf("insert auth token: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", AuthToken{}, fmt.Errorf("commit auth token: %w", err)
	}

	return rawToken, AuthToken{
		TokenHash:      hash,
		TokenPrefix:    prefix,
		TelegramUserID: telegramUserID,
		CreatedAt:      nowUnix,
		ExpiresAt:      expiresUnix,
	}, nil
}

// ValidateAuthToken validates a raw Bearer token against its stored SHA-256 hash,
// checking revocation and expiration at the given time.
func (s *Store) ValidateAuthToken(ctx context.Context, rawToken string, now time.Time) (int64, error) {
	if strings.TrimSpace(rawToken) == "" {
		return 0, ErrInvalidToken
	}

	hash := hashToken(rawToken)
	var userID, expiresAt int64
	var revokedAt sql.NullInt64

	err := s.db.QueryRowContext(ctx, `
		SELECT telegram_user_id, expires_at, revoked_at
		FROM auth_tokens
		WHERE token_hash = ?
	`, hash).Scan(&userID, &expiresAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrInvalidToken
	}
	if err != nil {
		return 0, fmt.Errorf("validate auth token: %w", err)
	}
	if revokedAt.Valid || (expiresAt > 0 && now.UTC().Unix() >= expiresAt) {
		return 0, ErrInvalidToken
	}
	return userID, nil
}

// RevokeUserTokens immediately revokes all active Bearer tokens, OAuth refresh tokens,
// and any pending OAuth sessions for telegramUserID.
func (s *Store) RevokeUserTokens(ctx context.Context, telegramUserID int64) (int64, error) {
	nowUnix := time.Now().UTC().Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE auth_tokens
		SET revoked_at = ?
		WHERE telegram_user_id = ? AND revoked_at IS NULL
	`, nowUnix, telegramUserID)
	if err != nil {
		return 0, fmt.Errorf("revoke user tokens %d: %w", telegramUserID, err)
	}
	authCount, _ := res.RowsAffected()

	refRes, err := s.db.ExecContext(ctx, `
		UPDATE oauth_refresh_tokens
		SET revoked_at = ?
		WHERE telegram_user_id = ? AND revoked_at IS NULL
	`, nowUnix, telegramUserID)
	if err != nil {
		return authCount, fmt.Errorf("revoke user refresh tokens %d: %w", telegramUserID, err)
	}
	refCount, _ := refRes.RowsAffected()

	_, _ = s.db.ExecContext(ctx, `
		UPDATE oauth_sessions
		SET status = 'denied'
		WHERE telegram_user_id = ? AND status IN ('pending', 'approved')
	`, telegramUserID)

	return authCount + refCount, nil
}

// PurgeExpiredMessages deletes messages and daily digests older than retentionDays relative to now.
// FTS5 index entries are removed automatically via the messages_ad trigger.
func (s *Store) PurgeExpiredMessages(ctx context.Context, retentionDays int, now time.Time) (int64, error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	cutoff := now.UTC().Add(-time.Duration(retentionDays) * 24 * time.Hour).Unix()
	res, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("purge expired messages: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM daily_digests WHERE period_end < ?`, cutoff); err != nil {
		return 0, fmt.Errorf("purge expired digests: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM message_chunks WHERE max_created_at < ?`, cutoff); err != nil {
		return 0, fmt.Errorf("purge expired message chunks: %w", err)
	}
	return res.RowsAffected()
}

func scanMessages(rows *sql.Rows) ([]Message, error) {
	var out []Message
	for rows.Next() {
		var m Message
		var senderID, replyTo, threadID, editedAt sql.NullInt64
		var attachJSON sql.NullString

		if err := rows.Scan(
			&m.ID,
			&m.WorkspaceChatID,
			&m.TelegramMessageID,
			&senderID,
			&m.SenderName,
			&m.SenderUsername,
			&m.Text,
			&replyTo,
			&threadID,
			&attachJSON,
			&m.CreatedAt,
			&editedAt,
		); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}

		if senderID.Valid {
			v := senderID.Int64
			m.SenderTGID = &v
		}
		if replyTo.Valid {
			v := replyTo.Int64
			m.ReplyToMessageID = &v
		}
		if threadID.Valid {
			v := threadID.Int64
			m.MessageThreadID = &v
		}
		if editedAt.Valid {
			v := editedAt.Int64
			m.EditedAt = &v
		}
		if attachJSON.Valid && attachJSON.String != "" {
			var meta AttachmentMeta
			if err := json.Unmarshal([]byte(attachJSON.String), &meta); err == nil {
				m.AttachmentMeta = &meta
			}
		}

		out = append(out, m)
	}
	return out, rows.Err()
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
