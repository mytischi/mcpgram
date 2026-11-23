-- MCP OAuth 2.1 tables (Dynamic Client Registration, PKCE login sessions, rotating refresh tokens)

CREATE TABLE oauth_clients (
    client_id TEXT PRIMARY KEY,
    client_name TEXT NOT NULL DEFAULT '',
    redirect_uris TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE oauth_sessions (
    login_nonce TEXT PRIMARY KEY,
    client_id TEXT NOT NULL REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
    redirect_uri TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT '',
    scope TEXT NOT NULL DEFAULT '',
    code_challenge TEXT NOT NULL,
    code_challenge_method TEXT NOT NULL,
    resource TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending',
    telegram_user_id INTEGER,
    auth_code_hash TEXT UNIQUE,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE INDEX idx_oauth_sessions_user ON oauth_sessions(telegram_user_id);
CREATE INDEX idx_oauth_sessions_expires ON oauth_sessions(expires_at);

CREATE TABLE oauth_refresh_tokens (
    token_hash TEXT PRIMARY KEY,
    client_id TEXT NOT NULL REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
    telegram_user_id INTEGER NOT NULL,
    scope TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    revoked_at INTEGER
);

CREATE INDEX idx_oauth_refresh_user ON oauth_refresh_tokens(telegram_user_id);
