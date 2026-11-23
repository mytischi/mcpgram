package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// OAuthClient represents a dynamically registered MCP OAuth 2.1 client (RFC 7591).
type OAuthClient struct {
	ClientID     string
	ClientName   string
	RedirectURIs []string
	CreatedAt    int64
}

// OAuthSession tracks a short-lived PKCE authorization flow awaiting Telegram bot approval.
type OAuthSession struct {
	LoginNonce          string
	ClientID            string
	RedirectURI         string
	State               string
	Scope               string
	CodeChallenge       string
	CodeChallengeMethod string
	Resource            string
	Status              string // pending | approved | denied | exchanged
	TelegramUserID      *int64
	AuthCodeHash        string
	CreatedAt           int64
	ExpiresAt           int64
}

// OAuthRefreshToken represents a hashed rotating refresh token.
type OAuthRefreshToken struct {
	TokenHash      string
	ClientID       string
	TelegramUserID int64
	Scope          string
	CreatedAt      int64
	ExpiresAt      int64
	RevokedAt      *int64
}

// RegisterOAuthClient persists a newly registered dynamic OAuth client.
func (s *Store) RegisterOAuthClient(ctx context.Context, clientName string, redirectURIs []string) (OAuthClient, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return OAuthClient{}, fmt.Errorf("generate client_id entropy: %w", err)
	}
	clientID := "mcpc_" + hex.EncodeToString(buf)
	now := time.Now().UTC().Unix()

	urisJSON, err := json.Marshal(redirectURIs)
	if err != nil {
		return OAuthClient{}, fmt.Errorf("marshal redirect_uris: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO oauth_clients (client_id, client_name, redirect_uris, created_at)
		VALUES (?, ?, ?, ?)
	`, clientID, clientName, string(urisJSON), now)
	if err != nil {
		return OAuthClient{}, fmt.Errorf("insert oauth_client: %w", err)
	}

	return OAuthClient{
		ClientID:     clientID,
		ClientName:   clientName,
		RedirectURIs: redirectURIs,
		CreatedAt:    now,
	}, nil
}

// GetOAuthClient loads a registered OAuth client by client_id.
func (s *Store) GetOAuthClient(ctx context.Context, clientID string) (OAuthClient, error) {
	var c OAuthClient
	var urisRaw string
	err := s.db.QueryRowContext(ctx, `
		SELECT client_id, client_name, redirect_uris, created_at
		FROM oauth_clients
		WHERE client_id = ?
	`, clientID).Scan(&c.ClientID, &c.ClientName, &urisRaw, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthClient{}, ErrNotFound
	}
	if err != nil {
		return OAuthClient{}, fmt.Errorf("get oauth_client: %w", err)
	}
	_ = json.Unmarshal([]byte(urisRaw), &c.RedirectURIs)
	return c, nil
}

// CreateOAuthSession creates a pending 5-minute OAuth PKCE session and returns its login_nonce.
func (s *Store) CreateOAuthSession(ctx context.Context, sess OAuthSession, ttl time.Duration) (OAuthSession, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return OAuthSession{}, fmt.Errorf("generate login nonce: %w", err)
	}
	// Telegram deep-link start parameter allows [A-Za-z0-9_-] up to 64 chars.
	sess.LoginNonce = base64.RawURLEncoding.EncodeToString(buf)
	sess.Status = "pending"
	now := time.Now().UTC()
	sess.CreatedAt = now.Unix()
	sess.ExpiresAt = now.Add(ttl).Unix()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO oauth_sessions (
			login_nonce, client_id, redirect_uri, state, scope,
			code_challenge, code_challenge_method, resource,
			status, created_at, expires_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)
	`,
		sess.LoginNonce, sess.ClientID, sess.RedirectURI, sess.State, sess.Scope,
		sess.CodeChallenge, sess.CodeChallengeMethod, sess.Resource,
		sess.CreatedAt, sess.ExpiresAt,
	)
	if err != nil {
		return OAuthSession{}, fmt.Errorf("insert oauth_session: %w", err)
	}
	return sess, nil
}

// GetOAuthSession loads an OAuth session by login_nonce.
func (s *Store) GetOAuthSession(ctx context.Context, nonce string) (OAuthSession, error) {
	return s.scanOAuthSession(s.db.QueryRowContext(ctx, `
		SELECT
			login_nonce, client_id, redirect_uri, state, scope,
			code_challenge, code_challenge_method, resource,
			status, telegram_user_id, COALESCE(auth_code_hash, ''),
			created_at, expires_at
		FROM oauth_sessions
		WHERE login_nonce = ?
	`, nonce))
}

// ApproveOAuthSession marks a pending, unexpired session as approved by telegramUserID.
func (s *Store) ApproveOAuthSession(ctx context.Context, nonce string, telegramUserID int64, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE oauth_sessions
		SET status = 'approved', telegram_user_id = ?
		WHERE login_nonce = ? AND status = 'pending' AND expires_at > ?
	`, telegramUserID, nonce, now.UTC().Unix())
	if err != nil {
		return fmt.Errorf("approve oauth_session: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DenyOAuthSession marks a pending session as denied.
func (s *Store) DenyOAuthSession(ctx context.Context, nonce string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE oauth_sessions
		SET status = 'denied'
		WHERE login_nonce = ? AND status = 'pending'
	`, nonce)
	if err != nil {
		return fmt.Errorf("deny oauth_session: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// IssueOAuthCodeForSession atomically generates a one-time authorization code for an approved
// session whose auth_code_hash has not yet been issued.
func (s *Store) IssueOAuthCodeForSession(ctx context.Context, nonce string, now time.Time) (rawCode string, sess OAuthSession, err error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", OAuthSession{}, fmt.Errorf("generate auth code: %w", err)
	}
	rawCode = "mcpcode_" + base64.RawURLEncoding.EncodeToString(buf)
	codeHash := hashToken(rawCode)

	res, err := s.db.ExecContext(ctx, `
		UPDATE oauth_sessions
		SET auth_code_hash = ?
		WHERE login_nonce = ?
		  AND status = 'approved'
		  AND auth_code_hash IS NULL
		  AND expires_at > ?
	`, codeHash, nonce, now.UTC().Unix())
	if err != nil {
		return "", OAuthSession{}, fmt.Errorf("issue oauth auth_code: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return "", OAuthSession{}, ErrNotFound
	}

	sess, err = s.GetOAuthSession(ctx, nonce)
	if err != nil {
		return "", OAuthSession{}, err
	}
	return rawCode, sess, nil
}

// ConsumeOAuthCode atomically consumes a one-time authorization code (marking status = 'exchanged')
// and returns the session for PKCE verification.
func (s *Store) ConsumeOAuthCode(ctx context.Context, rawCode string, now time.Time) (OAuthSession, error) {
	codeHash := hashToken(rawCode)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OAuthSession{}, fmt.Errorf("begin consume oauth code: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	sess, err := s.scanOAuthSession(tx.QueryRowContext(ctx, `
		SELECT
			login_nonce, client_id, redirect_uri, state, scope,
			code_challenge, code_challenge_method, resource,
			status, telegram_user_id, COALESCE(auth_code_hash, ''),
			created_at, expires_at
		FROM oauth_sessions
		WHERE auth_code_hash = ?
	`, codeHash))
	if err != nil {
		return OAuthSession{}, err
	}
	if sess.Status != "approved" || now.UTC().Unix() >= sess.ExpiresAt || sess.TelegramUserID == nil {
		return OAuthSession{}, ErrInvalidToken
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE oauth_sessions SET status = 'exchanged' WHERE login_nonce = ?
	`, sess.LoginNonce); err != nil {
		return OAuthSession{}, fmt.Errorf("mark oauth session exchanged: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OAuthSession{}, fmt.Errorf("commit consume oauth code: %w", err)
	}
	return sess, nil
}

// IssueOAuthRefreshToken creates a hashed refresh token bound to (client_id, telegram_user_id).
func (s *Store) IssueOAuthRefreshToken(ctx context.Context, clientID string, telegramUserID int64, scope string, ttl time.Duration) (string, OAuthRefreshToken, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", OAuthRefreshToken{}, fmt.Errorf("generate refresh token: %w", err)
	}
	raw := "tgref_" + hex.EncodeToString(buf)
	hash := hashToken(raw)
	now := time.Now().UTC()
	var expiresUnix int64
	if ttl > 0 {
		expiresUnix = now.Add(ttl).Unix()
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO oauth_refresh_tokens (
			token_hash, client_id, telegram_user_id, scope, created_at, expires_at
		) VALUES (?, ?, ?, ?, ?, ?)
	`, hash, clientID, telegramUserID, scope, now.Unix(), expiresUnix)
	if err != nil {
		return "", OAuthRefreshToken{}, fmt.Errorf("insert oauth refresh token: %w", err)
	}

	return raw, OAuthRefreshToken{
		TokenHash:      hash,
		ClientID:       clientID,
		TelegramUserID: telegramUserID,
		Scope:          scope,
		CreatedAt:      now.Unix(),
		ExpiresAt:      expiresUnix,
	}, nil
}

// RotateOAuthRefreshToken validates rawRefreshToken, revokes it, and issues a fresh replacement
// refresh token (OAuth 2.1 refresh token rotation).
func (s *Store) RotateOAuthRefreshToken(ctx context.Context, rawRefreshToken, clientID string, ttl time.Duration, now time.Time) (newRaw string, rec OAuthRefreshToken, err error) {
	oldHash := hashToken(rawRefreshToken)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", OAuthRefreshToken{}, fmt.Errorf("begin rotate refresh token: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var storedClientID, scope string
	var userID, expiresAt int64
	var revokedAt sql.NullInt64

	err = tx.QueryRowContext(ctx, `
		SELECT client_id, telegram_user_id, scope, expires_at, revoked_at
		FROM oauth_refresh_tokens
		WHERE token_hash = ?
	`, oldHash).Scan(&storedClientID, &userID, &scope, &expiresAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", OAuthRefreshToken{}, ErrInvalidToken
	}
	if err != nil {
		return "", OAuthRefreshToken{}, fmt.Errorf("lookup refresh token: %w", err)
	}

	nowUnix := now.UTC().Unix()
	if revokedAt.Valid || storedClientID != clientID || (expiresAt > 0 && nowUnix >= expiresAt) {
		return "", OAuthRefreshToken{}, ErrInvalidToken
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE oauth_refresh_tokens SET revoked_at = ? WHERE token_hash = ?
	`, nowUnix, oldHash); err != nil {
		return "", OAuthRefreshToken{}, fmt.Errorf("revoke rotated refresh token: %w", err)
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", OAuthRefreshToken{}, fmt.Errorf("generate new refresh token: %w", err)
	}
	newRaw = "tgref_" + hex.EncodeToString(buf)
	newHash := hashToken(newRaw)
	var newExpires int64
	if ttl > 0 {
		newExpires = now.UTC().Add(ttl).Unix()
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO oauth_refresh_tokens (
			token_hash, client_id, telegram_user_id, scope, created_at, expires_at
		) VALUES (?, ?, ?, ?, ?, ?)
	`, newHash, clientID, userID, scope, nowUnix, newExpires); err != nil {
		return "", OAuthRefreshToken{}, fmt.Errorf("insert rotated refresh token: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", OAuthRefreshToken{}, fmt.Errorf("commit rotate refresh token: %w", err)
	}

	return newRaw, OAuthRefreshToken{
		TokenHash:      newHash,
		ClientID:       clientID,
		TelegramUserID: userID,
		Scope:          scope,
		CreatedAt:      nowUnix,
		ExpiresAt:      newExpires,
	}, nil
}

func (s *Store) scanOAuthSession(row *sql.Row) (OAuthSession, error) {
	var sess OAuthSession
	var uid sql.NullInt64
	err := row.Scan(
		&sess.LoginNonce, &sess.ClientID, &sess.RedirectURI, &sess.State, &sess.Scope,
		&sess.CodeChallenge, &sess.CodeChallengeMethod, &sess.Resource,
		&sess.Status, &uid, &sess.AuthCodeHash,
		&sess.CreatedAt, &sess.ExpiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthSession{}, ErrNotFound
	}
	if err != nil {
		return OAuthSession{}, fmt.Errorf("scan oauth_session: %w", err)
	}
	if uid.Valid {
		v := uid.Int64
		sess.TelegramUserID = &v
	}
	return sess, nil
}

// GetPrimaryBotUsername returns the username of the most recently updated bot, or empty string if none.
func (s *Store) GetPrimaryBotUsername(ctx context.Context) string {
	var username string
	if err := s.db.QueryRowContext(ctx, `
		SELECT username FROM bots WHERE username != '' ORDER BY updated_at DESC LIMIT 1
	`).Scan(&username); err != nil {
		return ""
	}
	return username
}
