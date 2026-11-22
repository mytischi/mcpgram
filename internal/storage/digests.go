package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mytischi/mcpgram/internal/config"
)

// UpsertDigest inserts or updates a daily digest for (workspace_chat_id, digest_date).
func (s *Store) UpsertDigest(ctx context.Context, d DailyDigest) error {
	now := time.Now().UTC().Unix()
	if d.CreatedAt == 0 {
		d.CreatedAt = now
	}
	d.UpdatedAt = now

	decisionsJSON, err := marshalStringSlice(d.KeyDecisions)
	if err != nil {
		return fmt.Errorf("marshal key_decisions: %w", err)
	}
	actionsJSON, err := marshalStringSlice(d.ActionItems)
	if err != nil {
		return fmt.Errorf("marshal action_items: %w", err)
	}
	topicsJSON, err := marshalStringSlice(d.Topics)
	if err != nil {
		return fmt.Errorf("marshal topics: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO daily_digests (
			workspace_chat_id, digest_date, period_start, period_end,
			message_count, last_message_id, summary,
			key_decisions, action_items, topics,
			model, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(workspace_chat_id, digest_date) DO UPDATE SET
			period_start = excluded.period_start,
			period_end = excluded.period_end,
			message_count = excluded.message_count,
			last_message_id = excluded.last_message_id,
			summary = excluded.summary,
			key_decisions = excluded.key_decisions,
			action_items = excluded.action_items,
			topics = excluded.topics,
			model = excluded.model,
			updated_at = excluded.updated_at
	`,
		d.WorkspaceChatID, d.DigestDate, d.PeriodStart, d.PeriodEnd,
		d.MessageCount, d.LastMessageID, d.Summary,
		decisionsJSON, actionsJSON, topicsJSON,
		d.Model, d.CreatedAt, d.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert digest %s/%s: %w", d.WorkspaceChatID, d.DigestDate, err)
	}
	return nil
}

// GetDigest loads a single digest record (used by the digest worker for idempotency checks).
func (s *Store) GetDigest(ctx context.Context, workspaceChatID, digestDate string) (DailyDigest, error) {
	var d DailyDigest
	var decisionsRaw, actionsRaw, topicsRaw string

	err := s.db.QueryRowContext(ctx, `
		SELECT
			workspace_chat_id, digest_date, period_start, period_end,
			message_count, last_message_id, summary,
			key_decisions, action_items, topics,
			model, created_at, updated_at
		FROM daily_digests
		WHERE workspace_chat_id = ? AND digest_date = ?
	`, workspaceChatID, digestDate).Scan(
		&d.WorkspaceChatID, &d.DigestDate, &d.PeriodStart, &d.PeriodEnd,
		&d.MessageCount, &d.LastMessageID, &d.Summary,
		&decisionsRaw, &actionsRaw, &topicsRaw,
		&d.Model, &d.CreatedAt, &d.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return DailyDigest{}, ErrNotFound
	}
	if err != nil {
		return DailyDigest{}, fmt.Errorf("get digest %s/%s: %w", workspaceChatID, digestDate, err)
	}
	d.KeyDecisions = unmarshalStringSlice(decisionsRaw)
	d.ActionItems = unmarshalStringSlice(actionsRaw)
	d.Topics = unmarshalStringSlice(topicsRaw)
	return d, nil
}

// GetDigests returns precomputed daily digests for a chat strictly filtered by RBAC,
// fail-closed ACL freshness, and history policy (under since_join, a day's digest is visible
// only if the entire day started on or after the user's joined_at timestamp).
func (s *Store) GetDigests(ctx context.Context, filter DigestFilter) ([]DailyDigest, error) {
	policy := filter.DefaultHistoryPolicy
	if !policy.Valid() {
		policy = config.HistoryPolicyFull
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 31
	}
	if limit > 90 {
		limit = 90
	}

	query := `
		SELECT
			d.workspace_chat_id, d.digest_date, d.period_start, d.period_end,
			d.message_count, d.last_message_id, d.summary,
			d.key_decisions, d.action_items, d.topics,
			d.model, d.created_at, d.updated_at
		FROM daily_digests d
		JOIN chats c ON c.workspace_chat_id = d.workspace_chat_id
		JOIN chat_acl acl
			ON acl.workspace_chat_id = d.workspace_chat_id
			AND acl.telegram_user_id = ?
		WHERE d.workspace_chat_id = ?
		  AND (
			acl.status IN ('creator', 'administrator', 'member')
			OR (acl.status = 'restricted' AND acl.is_member = 1)
		  )
		  AND (? <= 0 OR acl.checked_at >= ?)
		  AND (
			COALESCE(c.history_policy_override, ?) = 'full_history'
			OR acl.joined_at IS NULL
			OR d.period_start >= acl.joined_at
		  )
	`
	args := []any{filter.TelegramUserID, filter.WorkspaceChatID, filter.MinACLCheckedAt, filter.MinACLCheckedAt, string(policy)}

	if filter.FromDate != "" {
		query += ` AND d.digest_date >= ?`
		args = append(args, filter.FromDate)
	}
	if filter.ToDate != "" {
		query += ` AND d.digest_date <= ?`
		args = append(args, filter.ToDate)
	}

	query += ` ORDER BY d.digest_date DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get digests: %w", err)
	}
	defer rows.Close()

	var out []DailyDigest
	for rows.Next() {
		var d DailyDigest
		var decisionsRaw, actionsRaw, topicsRaw string
		if err := rows.Scan(
			&d.WorkspaceChatID, &d.DigestDate, &d.PeriodStart, &d.PeriodEnd,
			&d.MessageCount, &d.LastMessageID, &d.Summary,
			&decisionsRaw, &actionsRaw, &topicsRaw,
			&d.Model, &d.CreatedAt, &d.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan digest: %w", err)
		}
		d.KeyDecisions = unmarshalStringSlice(decisionsRaw)
		d.ActionItems = unmarshalStringSlice(actionsRaw)
		d.Topics = unmarshalStringSlice(topicsRaw)
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetMessagesForPeriod loads chronological messages in [fromUnix, toUnix] for internal
// background processing (nightly digest worker). Not exposed directly to MCP clients.
func (s *Store) GetMessagesForPeriod(ctx context.Context, workspaceChatID string, fromUnix, toUnix int64) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			id, workspace_chat_id, telegram_message_id,
			sender_tg_id, sender_name, sender_username,
			text, reply_to_message_id, message_thread_id,
			attachment_meta, created_at, edited_at
		FROM messages
		WHERE workspace_chat_id = ?
		  AND created_at >= ?
		  AND created_at <= ?
		ORDER BY created_at ASC, telegram_message_id ASC
	`, workspaceChatID, fromUnix, toUnix)
	if err != nil {
		return nil, fmt.Errorf("get messages for period: %w", err)
	}
	defer rows.Close()

	return scanMessages(rows)
}

// ListTopicsMap returns a message_thread_id -> title map for a chat.
func (s *Store) ListTopicsMap(ctx context.Context, workspaceChatID string) (map[int64]string, error) {
	topics, err := s.listTopics(ctx, workspaceChatID)
	if err != nil {
		return nil, err
	}
	m := make(map[int64]string, len(topics))
	for _, t := range topics {
		m[t.MessageThreadID] = t.Title
	}
	return m, nil
}

func marshalStringSlice(items []string) (string, error) {
	if items == nil {
		items = []string{}
	}
	b, err := json.Marshal(items)
	if err != nil {
		return "[]", err
	}
	return string(b), nil
}

func unmarshalStringSlice(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}
