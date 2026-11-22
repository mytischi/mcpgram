package semantic

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/mytischi/mcpgram/internal/storage"
)

const (
	minChunkRunes = 15
	maxChunkChars = 2000
)

// DialogChunk represents a grouped window of chat messages ready for embedding.
type DialogChunk struct {
	WorkspaceChatID string
	MessageThreadID *int64
	FirstMessageID  int64
	LastMessageID   int64
	MinCreatedAt    int64
	MaxCreatedAt    int64
	Text            string
	ContentHash     string
}

// Key returns the stable range key matching storage.ListChunkHashesByChat.
func (c DialogChunk) Key() string {
	return fmt.Sprintf("%d:%d", c.FirstMessageID, c.LastMessageID)
}

// BuildDialogChunks groups chronological messages into meaningful conversational windows:
//   - keeps Forum Topics (message_thread_id) separate so parallel threads never mix;
//   - splits when time gap exceeds windowSec, message count hits maxMsgs, or size hits maxChunkChars;
//   - skips very short low-signal chunks (e.g. standalone "Ок" / "+1") shorter than 15 runes.
func BuildDialogChunks(msgs []storage.Message, windowSec, maxMsgs int) []DialogChunk {
	if len(msgs) == 0 {
		return nil
	}
	if windowSec <= 0 {
		windowSec = 600
	}
	if maxMsgs <= 0 {
		maxMsgs = 8
	}

	// Partition by message_thread_id (0 = main chat / no topic) while preserving order.
	type threadBucket struct {
		threadID *int64
		msgs     []storage.Message
	}
	buckets := make(map[int64]*threadBucket)
	var bucketOrder []int64

	for _, m := range msgs {
		text := strings.TrimSpace(m.Text)
		if text == "" {
			continue
		}
		var key int64
		if m.MessageThreadID != nil {
			key = *m.MessageThreadID
		}
		b, ok := buckets[key]
		if !ok {
			var tid *int64
			if m.MessageThreadID != nil {
				v := *m.MessageThreadID
				tid = &v
			}
			b = &threadBucket{threadID: tid}
			buckets[key] = b
			bucketOrder = append(bucketOrder, key)
		}
		b.msgs = append(b.msgs, m)
	}

	var chunks []DialogChunk
	for _, key := range bucketOrder {
		b := buckets[key]
		var group []storage.Message
		var curChars int

		flush := func() {
			if len(group) == 0 {
				return
			}
			var sb strings.Builder
			for i, m := range group {
				if i > 0 {
					sb.WriteByte('\n')
				}
				author := m.SenderName
				if author == "" {
					author = "user"
				}
				fmt.Fprintf(&sb, "%s: %s", author, strings.TrimSpace(m.Text))
			}
			fullText := sb.String()
			// Strip author prefixes when checking minimum signal length.
			var rawTextLen int
			for _, m := range group {
				rawTextLen += utf8.RuneCountInString(strings.TrimSpace(m.Text))
			}
			if rawTextLen >= minChunkRunes {
				sum := sha256.Sum256([]byte(fullText))
				chunks = append(chunks, DialogChunk{
					WorkspaceChatID: group[0].WorkspaceChatID,
					MessageThreadID: b.threadID,
					FirstMessageID:  group[0].TelegramMessageID,
					LastMessageID:   group[len(group)-1].TelegramMessageID,
					MinCreatedAt:    group[0].CreatedAt,
					MaxCreatedAt:    group[len(group)-1].CreatedAt,
					Text:            fullText,
					ContentHash:     hex.EncodeToString(sum[:]),
				})
			}
			group = nil
			curChars = 0
		}

		for _, m := range b.msgs {
			lineLen := len(m.SenderName) + len(m.Text) + 3
			if len(group) > 0 {
				prev := group[len(group)-1]
				gapExceeded := m.CreatedAt-prev.CreatedAt > int64(windowSec)
				if gapExceeded || len(group) >= maxMsgs || curChars+lineLen > maxChunkChars {
					flush()
				}
			}
			group = append(group, m)
			curChars += lineLen
		}
		flush()
	}

	return chunks
}
