-- Initial schema for mcpgram (SQLite WAL + FTS5)

CREATE TABLE bots (
    bot_id INTEGER PRIMARY KEY,
    username TEXT NOT NULL DEFAULT '',
    first_name TEXT NOT NULL DEFAULT '',
    can_read_all_group_messages INTEGER NOT NULL DEFAULT 0,
    update_offset INTEGER NOT NULL DEFAULT 0,
    last_poll_at INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE chats (
    workspace_chat_id TEXT PRIMARY KEY,
    telegram_chat_id INTEGER NOT NULL UNIQUE,
    assigned_bot_id INTEGER REFERENCES bots(bot_id),
    title TEXT NOT NULL DEFAULT '',
    chat_type TEXT NOT NULL DEFAULT '',
    is_forum INTEGER NOT NULL DEFAULT 0,
    bot_is_admin INTEGER NOT NULL DEFAULT 0,
    bot_status TEXT NOT NULL DEFAULT '',
    history_policy_override TEXT CHECK (
        history_policy_override IS NULL
        OR history_policy_override IN ('full_history', 'since_join')
    ),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE chat_id_aliases (
    telegram_chat_id INTEGER PRIMARY KEY,
    workspace_chat_id TEXT NOT NULL REFERENCES chats(workspace_chat_id) ON DELETE CASCADE,
    migrated_at INTEGER NOT NULL
);

CREATE INDEX idx_chat_id_aliases_workspace ON chat_id_aliases(workspace_chat_id);

CREATE TABLE topics (
    workspace_chat_id TEXT NOT NULL REFERENCES chats(workspace_chat_id) ON DELETE CASCADE,
    message_thread_id INTEGER NOT NULL,
    title TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (workspace_chat_id, message_thread_id)
);

CREATE TABLE messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    workspace_chat_id TEXT NOT NULL REFERENCES chats(workspace_chat_id) ON DELETE CASCADE,
    telegram_message_id INTEGER NOT NULL,
    sender_tg_id INTEGER,
    sender_name TEXT NOT NULL DEFAULT '',
    sender_username TEXT NOT NULL DEFAULT '',
    text TEXT NOT NULL DEFAULT '',
    reply_to_message_id INTEGER,
    message_thread_id INTEGER,
    attachment_meta TEXT,
    created_at INTEGER NOT NULL,
    edited_at INTEGER,
    UNIQUE (workspace_chat_id, telegram_message_id)
);

CREATE INDEX idx_messages_chat_created ON messages(workspace_chat_id, created_at);
CREATE INDEX idx_messages_chat_thread_created ON messages(workspace_chat_id, message_thread_id, created_at);
CREATE INDEX idx_messages_chat_reply ON messages(workspace_chat_id, reply_to_message_id);
CREATE INDEX idx_messages_created ON messages(created_at);

CREATE VIRTUAL TABLE messages_fts USING fts5(
    text,
    sender_name,
    content='messages',
    content_rowid='id',
    tokenize='unicode61 remove_diacritics 2'
);

CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, text, sender_name)
    VALUES (new.id, new.text, new.sender_name);
END;

CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, text, sender_name)
    VALUES ('delete', old.id, old.text, old.sender_name);
END;

CREATE TRIGGER messages_au AFTER UPDATE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, text, sender_name)
    VALUES ('delete', old.id, old.text, old.sender_name);
    INSERT INTO messages_fts(rowid, text, sender_name)
    VALUES (new.id, new.text, new.sender_name);
END;

CREATE TABLE chat_acl (
    workspace_chat_id TEXT NOT NULL REFERENCES chats(workspace_chat_id) ON DELETE CASCADE,
    telegram_user_id INTEGER NOT NULL,
    status TEXT NOT NULL,
    is_member INTEGER NOT NULL DEFAULT 0,
    joined_at INTEGER,
    checked_at INTEGER NOT NULL,
    PRIMARY KEY (workspace_chat_id, telegram_user_id)
);

CREATE INDEX idx_chat_acl_user ON chat_acl(telegram_user_id, workspace_chat_id);

CREATE TABLE auth_tokens (
    token_hash TEXT PRIMARY KEY,
    token_prefix TEXT NOT NULL,
    telegram_user_id INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    revoked_at INTEGER
);

CREATE INDEX idx_auth_tokens_user ON auth_tokens(telegram_user_id);
