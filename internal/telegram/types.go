package telegram

import (
	"fmt"
	"strings"

	"github.com/mytischi/mcpgram/internal/storage"
)

// APIResponse is the standard Telegram Bot API JSON envelope.
type APIResponse[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	ErrorCode   int    `json:"error_code,omitempty"`
	Description string `json:"description,omitempty"`
}

// User represents a Telegram user or bot.
type User struct {
	ID                      int64  `json:"id"`
	IsBot                   bool   `json:"is_bot"`
	FirstName               string `json:"first_name"`
	LastName                string `json:"last_name,omitempty"`
	Username                string `json:"username,omitempty"`
	CanReadAllGroupMessages bool   `json:"can_read_all_group_messages,omitempty"`
}

// DisplayName returns a human-readable display name for the user.
func (u User) DisplayName() string {
	full := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if full != "" {
		return full
	}
	if u.Username != "" {
		return "@" + u.Username
	}
	return fmt.Sprintf("user:%d", u.ID)
}

// Chat represents a Telegram chat.
type Chat struct {
	ID      int64  `json:"id"`
	Type    string `json:"type"`
	Title   string `json:"title,omitempty"`
	IsForum bool   `json:"is_forum,omitempty"`
}

// ChatMember represents a user's membership status in a chat.
type ChatMember struct {
	Status   string `json:"status"`
	User     User   `json:"user"`
	IsMember *bool  `json:"is_member,omitempty"`
}

// EffectiveIsMember returns whether the member is currently inside the chat.
// For "restricted" members Telegram populates IsMember explicitly.
func (m ChatMember) EffectiveIsMember() bool {
	if m.IsMember != nil {
		return *m.IsMember
	}
	switch strings.ToLower(strings.TrimSpace(m.Status)) {
	case "creator", "administrator", "member":
		return true
	default:
		return false
	}
}

// ChatMemberUpdated represents a chat_member or my_chat_member event.
type ChatMemberUpdated struct {
	Chat          Chat       `json:"chat"`
	From          User       `json:"from"`
	Date          int64      `json:"date"`
	OldChatMember ChatMember `json:"old_chat_member"`
	NewChatMember ChatMember `json:"new_chat_member"`
}

// ForumTopicCreated is a service message when a forum topic is created.
type ForumTopicCreated struct {
	Name string `json:"name"`
}

// ForumTopicEdited is a service message when a forum topic is edited.
type ForumTopicEdited struct {
	Name string `json:"name,omitempty"`
}

// PhotoSize represents one size of a photo.
type PhotoSize struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size,omitempty"`
}

// Document represents a general file attachment.
type Document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

// Audio represents an audio file attachment.
type Audio struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

// Video represents a video file attachment.
type Video struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

// Voice represents a voice note attachment.
type Voice struct {
	FileID   string `json:"file_id"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

// VideoNote represents a circular video note attachment.
type VideoNote struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size,omitempty"`
}

// Animation represents a GIF or H.264/MPEG-4 AVC animation.
type Animation struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

// Sticker represents a sticker attachment.
type Sticker struct {
	FileID   string `json:"file_id"`
	Emoji    string `json:"emoji,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

// Message represents a Telegram message or edited message.
type Message struct {
	MessageID         int64              `json:"message_id"`
	MessageThreadID   *int64             `json:"message_thread_id,omitempty"`
	From              *User              `json:"from,omitempty"`
	Chat              Chat               `json:"chat"`
	Date              int64              `json:"date"`
	EditDate          *int64             `json:"edit_date,omitempty"`
	ReplyToMessage    *Message           `json:"reply_to_message,omitempty"`
	Text              string             `json:"text,omitempty"`
	Caption           string             `json:"caption,omitempty"`
	MigrateToChatID   int64              `json:"migrate_to_chat_id,omitempty"`
	MigrateFromChatID int64              `json:"migrate_from_chat_id,omitempty"`
	ForumTopicCreated *ForumTopicCreated `json:"forum_topic_created,omitempty"`
	ForumTopicEdited  *ForumTopicEdited  `json:"forum_topic_edited,omitempty"`
	Photo             []PhotoSize        `json:"photo,omitempty"`
	Document          *Document          `json:"document,omitempty"`
	Audio             *Audio             `json:"audio,omitempty"`
	Video             *Video             `json:"video,omitempty"`
	Voice             *Voice             `json:"voice,omitempty"`
	VideoNote         *VideoNote         `json:"video_note,omitempty"`
	Animation         *Animation         `json:"animation,omitempty"`
	Sticker           *Sticker           `json:"sticker,omitempty"`
}

// EffectiveText returns message text or media caption.
func (m Message) EffectiveText() string {
	if m.Text != "" {
		return m.Text
	}
	return m.Caption
}

// ExtractAttachmentMeta extracts lightweight metadata for non-text attachments
// without downloading media binaries.
func (m Message) ExtractAttachmentMeta() *storage.AttachmentMeta {
	switch {
	case m.Document != nil:
		return &storage.AttachmentMeta{
			Type:     "document",
			FileID:   m.Document.FileID,
			FileName: m.Document.FileName,
			MimeType: m.Document.MimeType,
			FileSize: m.Document.FileSize,
		}
	case len(m.Photo) > 0:
		// Telegram sorts photo sizes from smallest to largest.
		best := m.Photo[len(m.Photo)-1]
		return &storage.AttachmentMeta{
			Type:     "photo",
			FileID:   best.FileID,
			FileSize: best.FileSize,
		}
	case m.Voice != nil:
		return &storage.AttachmentMeta{
			Type:     "voice",
			FileID:   m.Voice.FileID,
			MimeType: m.Voice.MimeType,
			FileSize: m.Voice.FileSize,
		}
	case m.VideoNote != nil:
		return &storage.AttachmentMeta{
			Type:     "video_note",
			FileID:   m.VideoNote.FileID,
			FileSize: m.VideoNote.FileSize,
		}
	case m.Video != nil:
		return &storage.AttachmentMeta{
			Type:     "video",
			FileID:   m.Video.FileID,
			FileName: m.Video.FileName,
			MimeType: m.Video.MimeType,
			FileSize: m.Video.FileSize,
		}
	case m.Audio != nil:
		return &storage.AttachmentMeta{
			Type:     "audio",
			FileID:   m.Audio.FileID,
			FileName: m.Audio.FileName,
			MimeType: m.Audio.MimeType,
			FileSize: m.Audio.FileSize,
		}
	case m.Animation != nil:
		return &storage.AttachmentMeta{
			Type:     "animation",
			FileID:   m.Animation.FileID,
			FileName: m.Animation.FileName,
			MimeType: m.Animation.MimeType,
			FileSize: m.Animation.FileSize,
		}
	case m.Sticker != nil:
		return &storage.AttachmentMeta{
			Type:     "sticker",
			FileID:   m.Sticker.FileID,
			FileSize: m.Sticker.FileSize,
		}
	default:
		return nil
	}
}

// Update represents an incoming Telegram update.
type Update struct {
	UpdateID      int64              `json:"update_id"`
	Message       *Message           `json:"message,omitempty"`
	EditedMessage *Message           `json:"edited_message,omitempty"`
	MyChatMember  *ChatMemberUpdated `json:"my_chat_member,omitempty"`
	ChatMember    *ChatMemberUpdated `json:"chat_member,omitempty"`
}

// BotCommand represents a command shown in Telegram's bot menu.
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// File represents a file ready to be downloaded via Telegram Bot API getFile.
type File struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileSize     int64  `json:"file_size,omitempty"`
	FilePath     string `json:"file_path,omitempty"`
}
