package storage

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/mytischi/mcpgram/internal/config"
)

// Searcher abstracts message search so v0.2+ can swap or wrap SQLite FTS5
// with hybrid FTS + vector RRF without changing the MCP tool contract.
type Searcher interface {
	SearchMessages(ctx context.Context, params SearchParams) ([]Message, error)
}

var _ Searcher = (*Store)(nil)

// BuildFTSQuery converts arbitrary user search text into a safe FTS5 MATCH expression.
// Each Unicode word/number token is quoted so punctuation (-, :, ", *, etc.) cannot trigger
// FTS5 syntax errors.
func BuildFTSQuery(raw string) string {
	var tokens []string
	var cur strings.Builder

	flush := func() {
		if cur.Len() == 0 {
			return
		}
		tok := cur.String()
		cur.Reset()
		// Escape any double quote defensively and wrap token in quotes.
		tok = strings.ReplaceAll(tok, `"`, `""`)
		tokens = append(tokens, `"`+tok+`"`)
	}

	for _, r := range raw {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()

	return strings.Join(tokens, " ")
}

// SearchMessages executes an RBAC- and history-policy-filtered FTS5 search.
func (s *Store) SearchMessages(ctx context.Context, params SearchParams) ([]Message, error) {
	ftsQuery := BuildFTSQuery(params.Query)
	if ftsQuery == "" {
		return nil, nil
	}

	policy := params.DefaultHistoryPolicy
	if !policy.Valid() {
		policy = config.HistoryPolicyFull
	}

	limit := params.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
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
		FROM messages_fts fts
		JOIN messages m ON m.id = fts.rowid
		JOIN chats c ON c.workspace_chat_id = m.workspace_chat_id
		JOIN chat_acl acl
			ON acl.workspace_chat_id = m.workspace_chat_id
			AND acl.telegram_user_id = ?
		WHERE messages_fts MATCH ?
		  AND (
			acl.status IN ('creator', 'administrator', 'member')
			OR (acl.status = 'restricted' AND acl.is_member = 1)
		  )
		  AND (
			COALESCE(c.history_policy_override, ?) = 'full_history'
			OR acl.joined_at IS NULL
			OR m.created_at >= acl.joined_at
		  )
	`
	args := []any{params.TelegramUserID, ftsQuery, string(policy)}

	if params.WorkspaceChatID != "" {
		query += ` AND m.workspace_chat_id = ?`
		args = append(args, params.WorkspaceChatID)
	}
	if params.TopicID != nil {
		query += ` AND m.message_thread_id = ?`
		args = append(args, *params.TopicID)
	}

	query += ` ORDER BY bm25(messages_fts), m.created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	defer rows.Close()

	return scanMessages(rows)
}
