-- Nightly chat digests table (v0.2)

CREATE TABLE daily_digests (
    workspace_chat_id TEXT NOT NULL REFERENCES chats(workspace_chat_id) ON DELETE CASCADE,
    digest_date TEXT NOT NULL,
    period_start INTEGER NOT NULL,
    period_end INTEGER NOT NULL,
    message_count INTEGER NOT NULL,
    last_message_id INTEGER NOT NULL,
    summary TEXT NOT NULL,
    key_decisions TEXT NOT NULL DEFAULT '[]',
    action_items TEXT NOT NULL DEFAULT '[]',
    topics TEXT NOT NULL DEFAULT '[]',
    model TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (workspace_chat_id, digest_date)
);

CREATE INDEX idx_daily_digests_chat_date ON daily_digests(workspace_chat_id, digest_date);
CREATE INDEX idx_daily_digests_period_end ON daily_digests(period_end);
