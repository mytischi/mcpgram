package storage

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/mytischi/mcpgram/internal/config"
)

// MessageChunk stores an embedded conversational window of messages for hybrid semantic search.
type MessageChunk struct {
	ID              int64
	WorkspaceChatID string
	MessageThreadID *int64
	FirstMessageID  int64
	LastMessageID   int64
	MinCreatedAt    int64
	MaxCreatedAt    int64
	ChunkText       string
	ContentHash     string
	Model           string
	Dimensions      int
	Embedding       []float32
	CreatedAt       int64
	UpdatedAt       int64
}

// UpsertMessageChunk inserts or updates a normalized float32 embedding chunk.
func (s *Store) UpsertMessageChunk(ctx context.Context, chunk MessageChunk) error {
	now := time.Now().UTC().Unix()
	if chunk.CreatedAt == 0 {
		chunk.CreatedAt = now
	}
	chunk.UpdatedAt = now

	normalized := NormalizeVector(chunk.Embedding)
	blob := EncodeFloat32s(normalized)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO message_chunks (
			workspace_chat_id, message_thread_id, first_message_id, last_message_id,
			min_created_at, max_created_at, chunk_text, content_hash,
			model, dimensions, embedding, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(workspace_chat_id, first_message_id, last_message_id, model) DO UPDATE SET
			message_thread_id = excluded.message_thread_id,
			min_created_at = excluded.min_created_at,
			max_created_at = excluded.max_created_at,
			chunk_text = excluded.chunk_text,
			content_hash = excluded.content_hash,
			dimensions = excluded.dimensions,
			embedding = excluded.embedding,
			updated_at = excluded.updated_at
	`,
		chunk.WorkspaceChatID,
		chunk.MessageThreadID,
		chunk.FirstMessageID,
		chunk.LastMessageID,
		chunk.MinCreatedAt,
		chunk.MaxCreatedAt,
		chunk.ChunkText,
		chunk.ContentHash,
		chunk.Model,
		len(normalized),
		blob,
		chunk.CreatedAt,
		chunk.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert message chunk: %w", err)
	}
	return nil
}

// ListChunkHashesByChat returns existing (first_message_id, last_message_id) -> content_hash
// for idempotent semantic indexing.
func (s *Store) ListChunkHashesByChat(ctx context.Context, workspaceChatID, model string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT first_message_id, last_message_id, content_hash
		FROM message_chunks
		WHERE workspace_chat_id = ? AND model = ?
	`, workspaceChatID, model)
	if err != nil {
		return nil, fmt.Errorf("list chunk hashes: %w", err)
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var firstID, lastID int64
		var hash string
		if err := rows.Scan(&firstID, &lastID, &hash); err != nil {
			return nil, err
		}
		out[fmt.Sprintf("%d:%d", firstID, lastID)] = hash
	}
	return out, rows.Err()
}

// ListChatMessagesAll loads all messages in a chat chronologically for background chunk indexing.
func (s *Store) ListChatMessagesAll(ctx context.Context, workspaceChatID string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			id, workspace_chat_id, telegram_message_id,
			sender_tg_id, sender_name, sender_username,
			text, reply_to_message_id, message_thread_id,
			attachment_meta, created_at, edited_at
		FROM messages
		WHERE workspace_chat_id = ?
		ORDER BY created_at ASC, telegram_message_id ASC
	`, workspaceChatID)
	if err != nil {
		return nil, fmt.Errorf("list all chat messages: %w", err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

// ListAccessibleChunksPreFiltered returns ONLY chunks that pass strict RBAC, fail-closed ACL
// freshness, and history_policy (since_join checks min_created_at >= joined_at) BEFORE vector scoring.
func (s *Store) ListAccessibleChunksPreFiltered(ctx context.Context, params SearchParams, model string) ([]MessageChunk, error) {
	policy := params.DefaultHistoryPolicy
	if !policy.Valid() {
		policy = config.HistoryPolicyFull
	}

	query := `
		SELECT
			mc.id,
			mc.workspace_chat_id,
			mc.message_thread_id,
			mc.first_message_id,
			mc.last_message_id,
			mc.min_created_at,
			mc.max_created_at,
			mc.chunk_text,
			mc.content_hash,
			mc.model,
			mc.dimensions,
			mc.embedding,
			mc.created_at,
			mc.updated_at
		FROM message_chunks mc
		JOIN chats c ON c.workspace_chat_id = mc.workspace_chat_id
		JOIN chat_acl acl
			ON acl.workspace_chat_id = mc.workspace_chat_id
			AND acl.telegram_user_id = ?
		WHERE mc.model = ?
		  AND (
			acl.status IN ('creator', 'administrator', 'member')
			OR (acl.status = 'restricted' AND acl.is_member = 1)
		  )
		  AND (? <= 0 OR acl.checked_at >= ?)
		  AND (
			COALESCE(c.history_policy_override, ?) = 'full_history'
			OR acl.joined_at IS NULL
			OR mc.min_created_at >= acl.joined_at
		  )
	`
	args := []any{params.TelegramUserID, model, params.MinACLCheckedAt, params.MinACLCheckedAt, string(policy)}

	if params.WorkspaceChatID != "" {
		query += ` AND mc.workspace_chat_id = ?`
		args = append(args, params.WorkspaceChatID)
	}
	if params.TopicID != nil {
		query += ` AND mc.message_thread_id = ?`
		args = append(args, *params.TopicID)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list accessible chunks: %w", err)
	}
	defer rows.Close()

	var out []MessageChunk
	for rows.Next() {
		var mc MessageChunk
		var threadID sql.NullInt64
		var blob []byte
		if err := rows.Scan(
			&mc.ID, &mc.WorkspaceChatID, &threadID,
			&mc.FirstMessageID, &mc.LastMessageID,
			&mc.MinCreatedAt, &mc.MaxCreatedAt,
			&mc.ChunkText, &mc.ContentHash,
			&mc.Model, &mc.Dimensions, &blob,
			&mc.CreatedAt, &mc.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan message chunk: %w", err)
		}
		if threadID.Valid {
			v := threadID.Int64
			mc.MessageThreadID = &v
		}
		mc.Embedding = DecodeFloat32s(blob)
		out = append(out, mc)
	}
	return out, rows.Err()
}

// GetMessagesByIDRange loads RBAC-filtered messages within [firstMsgID, lastMsgID] for a chat
// (used to expand matched semantic chunks back into standard Message records).
func (s *Store) GetMessagesByIDRange(ctx context.Context, params SearchParams, workspaceChatID string, firstMsgID, lastMsgID int64) ([]Message, error) {
	policy := params.DefaultHistoryPolicy
	if !policy.Valid() {
		policy = config.HistoryPolicyFull
	}

	rows, err := s.db.QueryContext(ctx, `
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
		  AND m.telegram_message_id >= ?
		  AND m.telegram_message_id <= ?
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
		ORDER BY m.created_at ASC, m.telegram_message_id ASC
	`,
		params.TelegramUserID, workspaceChatID, firstMsgID, lastMsgID,
		params.MinACLCheckedAt, params.MinACLCheckedAt, string(policy),
	)
	if err != nil {
		return nil, fmt.Errorf("get messages by id range: %w", err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

// NormalizeVector returns an L2-normalized copy of v so cosine similarity reduces to dot product.
func NormalizeVector(v []float32) []float32 {
	out := make([]float32, len(v))
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return out
	}
	inv := float32(1.0 / math.Sqrt(sum))
	for i, x := range v {
		out[i] = x * inv
	}
	return out
}

// DotProduct computes the dot product of two equal-length float32 vectors.
func DotProduct(a, b []float32) float32 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var sum float32
	for i := range a {
		sum += a[i] * b[i]
	}
	return sum
}

// EncodeFloat32s packs a float32 slice into little-endian bytes for compact SQLite BLOB storage.
func EncodeFloat32s(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, x := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(x))
	}
	return buf
}

// DecodeFloat32s unpacks little-endian bytes back into a float32 slice.
func DecodeFloat32s(b []byte) []float32 {
	if len(b)%4 != 0 {
		return nil
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}
