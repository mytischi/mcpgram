-- Semantic dialog chunks (vector embeddings) & Whisper transcription tracking (v0.2)

CREATE TABLE message_chunks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    workspace_chat_id TEXT NOT NULL REFERENCES chats(workspace_chat_id) ON DELETE CASCADE,
    message_thread_id INTEGER,
    first_message_id INTEGER NOT NULL,
    last_message_id INTEGER NOT NULL,
    min_created_at INTEGER NOT NULL,
    max_created_at INTEGER NOT NULL,
    chunk_text TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    model TEXT NOT NULL,
    dimensions INTEGER NOT NULL,
    embedding BLOB NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (workspace_chat_id, first_message_id, last_message_id, model)
);

CREATE INDEX idx_message_chunks_chat_model ON message_chunks(workspace_chat_id, model, min_created_at);
CREATE INDEX idx_message_chunks_max_created ON message_chunks(max_created_at);

CREATE TABLE transcription_jobs (
    message_row_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    workspace_chat_id TEXT NOT NULL REFERENCES chats(workspace_chat_id) ON DELETE CASCADE,
    telegram_message_id INTEGER NOT NULL,
    file_id TEXT NOT NULL,
    media_type TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    attempts INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    transcript TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL
);

CREATE INDEX idx_transcription_jobs_status ON transcription_jobs(status, attempts, updated_at);
