package storage

import (
	"strings"

	"github.com/mytischi/mcpgram/internal/config"
)

// Bot represents a configured Telegram bot tracked by mcpgram.
type Bot struct {
	BotID                   int64
	Username                string
	FirstName               string
	CanReadAllGroupMessages bool
	UpdateOffset            int64
	LastPollAt              *int64
	CreatedAt               int64
	UpdatedAt               int64
}

// Chat represents a tracked Telegram group/supergroup identified by an internal UUID.
type Chat struct {
	WorkspaceChatID       string
	TelegramChatID        int64
	AssignedBotID         *int64
	Title                 string
	ChatType              string
	IsForum               bool
	BotIsAdmin            bool
	BotStatus             string
	HistoryPolicyOverride *config.HistoryPolicy
	CreatedAt             int64
	UpdatedAt             int64
}

// Topic represents a Forum Topic within a supergroup.
type Topic struct {
	WorkspaceChatID string
	MessageThreadID int64
	Title           string
	CreatedAt       int64
	UpdatedAt       int64
}

// AttachmentMeta stores lightweight metadata for non-text attachments without downloading binaries.
type AttachmentMeta struct {
	Type     string `json:"type"`
	FileID   string `json:"file_id,omitempty"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

// Message represents a persisted chat message.
type Message struct {
	ID                int64
	WorkspaceChatID   string
	TelegramMessageID int64
	SenderTGID        *int64
	SenderName        string
	SenderUsername    string
	Text              string
	ReplyToMessageID  *int64
	MessageThreadID   *int64
	AttachmentMeta    *AttachmentMeta
	CreatedAt         int64
	EditedAt          *int64
}

// ChatACL caches a Telegram user's membership status in a chat.
type ChatACL struct {
	WorkspaceChatID string
	TelegramUserID  int64
	Status          string
	IsMember        bool
	JoinedAt        *int64
	CheckedAt       int64
}

// AuthToken represents an employee's hashed Bearer token record.
type AuthToken struct {
	TokenHash      string
	TokenPrefix    string
	TelegramUserID int64
	CreatedAt      int64
	ExpiresAt      int64
	RevokedAt      *int64
}

// ChatSummary is returned by ListAccessibleChats for the MCP list_chats tool.
type ChatSummary struct {
	WorkspaceChatID string
	Title           string
	ChatType        string
	IsForum         bool
	Topics          []Topic
	LastMessageAt   *int64
	MessageCount    int64
}

// SearchParams configures an RBAC-filtered full-text (or hybrid in v0.2+) search query.
type SearchParams struct {
	TelegramUserID       int64
	WorkspaceChatID      string // optional: empty searches across all accessible chats
	Query                string
	TopicID              *int64
	DefaultHistoryPolicy config.HistoryPolicy
	MinACLCheckedAt      int64 // optional: when > 0, enforces fail-closed ACL freshness in SQL
	Limit                int
}

// MessageFilter configures an RBAC-filtered chronological message query.
type MessageFilter struct {
	TelegramUserID       int64
	WorkspaceChatID      string
	FromUnix             *int64
	ToUnix               *int64
	TopicID              *int64
	Author               string // optional match against sender_name or sender_username
	DefaultHistoryPolicy config.HistoryPolicy
	MinACLCheckedAt      int64 // optional: when > 0, enforces fail-closed ACL freshness in SQL
	Limit                int
}

// ThreadParams configures an RBAC-filtered reply-chain lookup.
type ThreadParams struct {
	TelegramUserID       int64
	WorkspaceChatID      string
	TelegramMessageID    int64
	DefaultHistoryPolicy config.HistoryPolicy
	MinACLCheckedAt      int64 // optional: when > 0, enforces fail-closed ACL freshness in SQL
	Limit                int
}

// IsAllowedMemberStatus enforces the strict Telegram membership allowlist:
// creator, administrator, member, or restricted ONLY when isMember == true.
func IsAllowedMemberStatus(status string, isMember bool) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "creator", "administrator", "member":
		return true
	case "restricted":
		return isMember
	default:
		return false
	}
}

// ShouldIgnoreMessage reports whether text contains the configured stop-tag (case-insensitive).
func ShouldIgnoreMessage(text, ignoreTag string) bool {
	ignoreTag = strings.TrimSpace(ignoreTag)
	if ignoreTag == "" || text == "" {
		return false
	}
	return strings.Contains(strings.ToLower(text), strings.ToLower(ignoreTag))
}
