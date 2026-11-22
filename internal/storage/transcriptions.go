package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// PendingTranscription represents a voice or video_note message waiting for Whisper transcription.
type PendingTranscription struct {
	MessageRowID      int64
	WorkspaceChatID   string
	TelegramMessageID int64
	FileID            string
	MediaType         string
	FileName          string
	ExistingText      string
	Attempts          int
}

// ListPendingTranscriptions discovers voice and video_note messages that have not yet been
// successfully transcribed (and have fewer than maxAttempts failures).
func (s *Store) ListPendingTranscriptions(ctx context.Context, maxAttempts, limit int) ([]PendingTranscription, error) {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	if limit <= 0 {
		limit = 10
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT
			m.id,
			m.workspace_chat_id,
			m.telegram_message_id,
			m.text,
			m.attachment_meta,
			COALESCE(tj.attempts, 0)
		FROM messages m
		LEFT JOIN transcription_jobs tj ON tj.message_row_id = m.id
		WHERE m.attachment_meta IS NOT NULL
		  AND (
			m.attachment_meta LIKE '%"type":"voice"%'
			OR m.attachment_meta LIKE '%"type":"video_note"%'
		  )
		  AND (tj.status IS NULL OR (tj.status != 'done' AND tj.attempts < ?))
		ORDER BY m.created_at ASC
		LIMIT ?
	`, maxAttempts, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending transcriptions: %w", err)
	}
	defer rows.Close()

	var out []PendingTranscription
	for rows.Next() {
		var item PendingTranscription
		var attachJSON sql.NullString
		if err := rows.Scan(
			&item.MessageRowID,
			&item.WorkspaceChatID,
			&item.TelegramMessageID,
			&item.ExistingText,
			&attachJSON,
			&item.Attempts,
		); err != nil {
			return nil, fmt.Errorf("scan pending transcription: %w", err)
		}
		if !attachJSON.Valid {
			continue
		}
		var meta AttachmentMeta
		if err := json.Unmarshal([]byte(attachJSON.String), &meta); err != nil {
			continue
		}
		if meta.FileID == "" || (meta.Type != "voice" && meta.Type != "video_note") {
			continue
		}
		item.FileID = meta.FileID
		item.MediaType = meta.Type
		item.FileName = meta.FileName
		out = append(out, item)
	}
	return out, rows.Err()
}

// CompleteTranscription stores a successful transcript in transcription_jobs AND appends it to
// messages.text (which automatically updates FTS5 via the messages_au trigger).
func (s *Store) CompleteTranscription(ctx context.Context, item PendingTranscription, transcript string) error {
	transcript = strings.TrimSpace(transcript)
	now := time.Now().UTC().Unix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin complete transcription: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO transcription_jobs (
			message_row_id, workspace_chat_id, telegram_message_id,
			file_id, media_type, status, attempts, last_error, transcript, updated_at
		) VALUES (?, ?, ?, ?, ?, 'done', ?, '', ?, ?)
		ON CONFLICT(message_row_id) DO UPDATE SET
			status = 'done',
			attempts = excluded.attempts,
			last_error = '',
			transcript = excluded.transcript,
			updated_at = excluded.updated_at
	`,
		item.MessageRowID, item.WorkspaceChatID, item.TelegramMessageID,
		item.FileID, item.MediaType, item.Attempts+1, transcript, now,
	); err != nil {
		return fmt.Errorf("upsert completed transcription_job: %w", err)
	}

	if transcript != "" {
		label := fmt.Sprintf("[%s transcript]: %s", item.MediaType, transcript)
		newText := label
		if existing := strings.TrimSpace(item.ExistingText); existing != "" {
			if !strings.Contains(existing, label) {
				newText = existing + "\n" + label
			} else {
				newText = existing
			}
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE messages SET text = ? WHERE id = ?
		`, newText, item.MessageRowID); err != nil {
			return fmt.Errorf("update message with transcript: %w", err)
		}
	}

	return tx.Commit()
}

// FailTranscription increments attempts and records the error so failing jobs back off and stop after maxAttempts.
func (s *Store) FailTranscription(ctx context.Context, item PendingTranscription, errMsg string) error {
	now := time.Now().UTC().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO transcription_jobs (
			message_row_id, workspace_chat_id, telegram_message_id,
			file_id, media_type, status, attempts, last_error, transcript, updated_at
		) VALUES (?, ?, ?, ?, ?, 'failed', ?, ?, '', ?)
		ON CONFLICT(message_row_id) DO UPDATE SET
			status = 'failed',
			attempts = transcription_jobs.attempts + 1,
			last_error = excluded.last_error,
			updated_at = excluded.updated_at
	`,
		item.MessageRowID, item.WorkspaceChatID, item.TelegramMessageID,
		item.FileID, item.MediaType, item.Attempts+1, errMsg, now,
	)
	if err != nil {
		return fmt.Errorf("record failed transcription: %w", err)
	}
	return nil
}
