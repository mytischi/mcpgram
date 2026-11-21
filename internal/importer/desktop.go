package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mytischi/mcpgram/internal/storage"
)

// Options configures a Telegram Desktop result.json import run.
type Options struct {
	TelegramChatIDOverride int64  // optional: overrides chat ID derived from result.json
	IgnoreTag              string // e.g. "#offrecord"
	RetentionDays          int    // 0 imports full archive regardless of age
	Now                    time.Time
}

// Stats summarizes the result of a desktop export import.
type Stats struct {
	WorkspaceChatID string
	TelegramChatID  int64
	ChatTitle       string
	Imported        int
	IgnoredOffrec   int
	SkippedOld      int
	SkippedNonMsg   int
}

type desktopExport struct {
	Name     string           `json:"name"`
	Type     string           `json:"type"`
	ID       int64            `json:"id"`
	Messages []desktopMessage `json:"messages"`
}

type desktopMessage struct {
	ID               int64        `json:"id"`
	Type             string       `json:"type"`
	Action           string       `json:"action,omitempty"`
	Title            string       `json:"title,omitempty"`
	Date             string       `json:"date"`
	DateUnixtime     string       `json:"date_unixtime"`
	Edited           string       `json:"edited,omitempty"`
	EditedUnixtime   string       `json:"edited_unixtime,omitempty"`
	From             string       `json:"from,omitempty"`
	FromID           string       `json:"from_id,omitempty"`
	ReplyToMessageID *int64       `json:"reply_to_message_id,omitempty"`
	Text             FlexibleText `json:"text"`
	Photo            string       `json:"photo,omitempty"`
	File             string       `json:"file,omitempty"`
	FileName         string       `json:"file_name,omitempty"`
	MediaType        string       `json:"media_type,omitempty"`
	MimeType         string       `json:"mime_type,omitempty"`
	FileSize         int64        `json:"file_size,omitempty"`
}

// FlexibleText unmarshals Telegram Desktop's polymorphic text field, which can be either
// a plain JSON string or an array mixing strings and entity objects ({"type":"link","text":"..."}).
type FlexibleText string

func (f *FlexibleText) UnmarshalJSON(data []byte) error {
	var plain string
	if err := json.Unmarshal(data, &plain); err == nil {
		*f = FlexibleText(plain)
		return nil
	}

	var parts []json.RawMessage
	if err := json.Unmarshal(data, &parts); err != nil {
		return fmt.Errorf("decode flexible text: %w", err)
	}

	var b strings.Builder
	for _, raw := range parts {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			b.WriteString(s)
			continue
		}
		var obj struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &obj); err == nil {
			b.WriteString(obj.Text)
		}
	}
	*f = FlexibleText(b.String())
	return nil
}

// ImportDesktopFile opens a Telegram Desktop JSON export file and imports it into store.
func ImportDesktopFile(ctx context.Context, store *storage.Store, filePath string, opts Options) (Stats, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return Stats{}, fmt.Errorf("open export file %q: %w", filePath, err)
	}
	defer f.Close()

	return ImportDesktopReader(ctx, store, f, opts)
}

// ImportDesktopReader parses a Telegram Desktop JSON export from r and imports messages into store.
func ImportDesktopReader(ctx context.Context, store *storage.Store, r io.Reader, opts Options) (Stats, error) {
	var exp desktopExport
	dec := json.NewDecoder(r)
	if err := dec.Decode(&exp); err != nil {
		return Stats{}, fmt.Errorf("parse telegram desktop export json: %w", err)
	}

	tgChatID, chatType := normalizeDesktopChatID(exp.ID, exp.Type)
	if opts.TelegramChatIDOverride != 0 {
		tgChatID = opts.TelegramChatIDOverride
	}
	if tgChatID == 0 {
		return Stats{}, fmt.Errorf("could not determine telegram chat_id from export (pass --chat-id explicitly)")
	}

	chat, err := store.UpsertChat(ctx, storage.Chat{
		TelegramChatID: tgChatID,
		Title:          exp.Name,
		ChatType:       chatType,
	})
	if err != nil {
		return Stats{}, fmt.Errorf("upsert imported chat: %w", err)
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var cutoffUnix int64
	if opts.RetentionDays > 0 {
		cutoffUnix = now.UTC().Add(-time.Duration(opts.RetentionDays) * 24 * time.Hour).Unix()
	}

	stats := Stats{
		WorkspaceChatID: chat.WorkspaceChatID,
		TelegramChatID:  chat.TelegramChatID,
		ChatTitle:       chat.Title,
	}

	for _, m := range exp.Messages {
		if m.Type == "service" {
			if m.Action == "topic_created" && m.Title != "" && m.ID > 0 {
				_ = store.UpsertTopic(ctx, storage.Topic{
					WorkspaceChatID: chat.WorkspaceChatID,
					MessageThreadID: m.ID,
					Title:           m.Title,
				})
			}
			stats.SkippedNonMsg++
			continue
		}
		if m.Type != "message" || m.ID <= 0 {
			stats.SkippedNonMsg++
			continue
		}

		createdAt, err := parseDesktopTimestamp(m.DateUnixtime, m.Date)
		if err != nil {
			stats.SkippedNonMsg++
			continue
		}
		if cutoffUnix > 0 && createdAt < cutoffUnix {
			stats.SkippedOld++
			continue
		}

		text := strings.TrimSpace(string(m.Text))
		attach := extractDesktopAttachment(m)
		if text == "" && attach == nil {
			stats.SkippedNonMsg++
			continue
		}

		var editedAt *int64
		if m.EditedUnixtime != "" || m.Edited != "" {
			if ts, err := parseDesktopTimestamp(m.EditedUnixtime, m.Edited); err == nil {
				editedAt = &ts
			}
		}

		ignored, err := store.UpsertMessage(ctx, storage.Message{
			WorkspaceChatID:   chat.WorkspaceChatID,
			TelegramMessageID: m.ID,
			SenderTGID:        parseDesktopFromID(m.FromID),
			SenderName:        m.From,
			Text:              text,
			ReplyToMessageID:  m.ReplyToMessageID,
			AttachmentMeta:    attach,
			CreatedAt:         createdAt,
			EditedAt:          editedAt,
		}, opts.IgnoreTag)
		if err != nil {
			return stats, fmt.Errorf("import message %d: %w", m.ID, err)
		}
		if ignored {
			stats.IgnoredOffrec++
		} else {
			stats.Imported++
		}
	}

	return stats, nil
}

// normalizeDesktopChatID converts Telegram Desktop's positive numeric IDs into Bot API chat IDs:
// supergroup/channel ID 12345 -> -1000000012345 (prefixed with -100), group ID 12345 -> -12345.
func normalizeDesktopChatID(rawID int64, exportType string) (int64, string) {
	t := strings.ToLower(strings.TrimSpace(exportType))
	chatType := "group"
	if strings.Contains(t, "supergroup") {
		chatType = "supergroup"
	} else if strings.Contains(t, "channel") {
		chatType = "channel"
	}

	if rawID <= 0 {
		return rawID, chatType
	}

	if chatType == "supergroup" || chatType == "channel" {
		prefixed, err := strconv.ParseInt("-100"+strconv.FormatInt(rawID, 10), 10, 64)
		if err == nil {
			return prefixed, chatType
		}
	}
	return -rawID, chatType
}

func parseDesktopFromID(raw string) *int64 {
	raw = strings.TrimSpace(raw)
	for _, prefix := range []string{"user", "channel"} {
		if strings.HasPrefix(raw, prefix) {
			if id, err := strconv.ParseInt(strings.TrimPrefix(raw, prefix), 10, 64); err == nil && id > 0 {
				return &id
			}
		}
	}
	return nil
}

func parseDesktopTimestamp(unixStr, isoStr string) (int64, error) {
	if s := strings.TrimSpace(unixStr); s != "" {
		if ts, err := strconv.ParseInt(s, 10, 64); err == nil && ts > 0 {
			return ts, nil
		}
	}
	if s := strings.TrimSpace(isoStr); s != "" {
		for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC().Unix(), nil
			}
		}
	}
	return 0, fmt.Errorf("missing valid timestamp")
}

func extractDesktopAttachment(m desktopMessage) *storage.AttachmentMeta {
	switch {
	case m.Photo != "":
		return &storage.AttachmentMeta{Type: "photo", FileName: m.Photo, FileSize: m.FileSize}
	case m.File != "" || m.MediaType != "":
		kind := m.MediaType
		if kind == "" {
			kind = "document"
		}
		name := m.FileName
		if name == "" {
			name = m.File
		}
		return &storage.AttachmentMeta{
			Type:     kind,
			FileName: name,
			MimeType: m.MimeType,
			FileSize: m.FileSize,
		}
	default:
		return nil
	}
}
