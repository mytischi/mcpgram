package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mytischi/mcpgram/internal/config"
	"github.com/mytischi/mcpgram/internal/storage"
)

const (
	loginSessionTTL = 5 * time.Minute
	defaultScope    = "mcp:read"
)

// AccessChecker verifies whether a user is still a member of at least one tracked corporate chat.
type AccessChecker interface {
	HasAnyTrackedChatAccess(ctx context.Context, telegramUserID int64) (bool, error)
}

// Server implements MCP OAuth 2.1 (RFC 9728, RFC 8414, RFC 7591, PKCE S256, Refresh Token Rotation)
// backed by Telegram bot deep-link confirmation.
type Server struct {
	store   *storage.Store
	cfg     config.Config
	access  AccessChecker
	logger  *slog.Logger
	nowFunc func() time.Time
}

// NewServer constructs an MCP OAuth 2.1 server.
func NewServer(store *storage.Store, cfg config.Config, access AccessChecker, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		store:   store,
		cfg:     cfg,
		access:  access,
		logger:  logger,
		nowFunc: time.Now,
	}
}

// RegisterRoutes mounts the RFC 9728, RFC 8414, RFC 7591, and OAuth 2.1 endpoints onto mux.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/.well-known/oauth-protected-resource", s.handleProtectedResourceMetadata)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", s.handleProtectedResourceMetadata)
	mux.HandleFunc("/.well-known/oauth-authorization-server", s.handleAuthServerMetadata)
	mux.HandleFunc("/oauth/register", s.handleRegister)
	mux.HandleFunc("/oauth/authorize", s.handleAuthorize)
	mux.HandleFunc("/oauth/status", s.handleStatus)
	mux.HandleFunc("/oauth/token", s.handleToken)
}

// ApproveLogin marks a pending browser login session as approved by telegramUserID.
func (s *Server) ApproveLogin(ctx context.Context, nonce string, telegramUserID int64) error {
	return s.store.ApproveOAuthSession(ctx, nonce, telegramUserID, s.nowFunc())
}

// DenyLogin marks a pending browser login session as denied.
func (s *Server) DenyLogin(ctx context.Context, nonce string) error {
	return s.store.DenyOAuthSession(ctx, nonce)
}

// ProtectedResourceMetadataURL returns the RFC 9728 metadata URL advertised in WWW-Authenticate.
func (s *Server) ProtectedResourceMetadataURL() string {
	return s.cfg.PublicBaseURL + "/.well-known/oauth-protected-resource"
}

func (s *Server) handleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSONError(w, http.StatusMethodNotAllowed, "invalid_request", "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 s.cfg.PublicBaseURL + "/mcp",
		"authorization_servers":    []string{s.cfg.PublicBaseURL},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         []string{defaultScope},
	})
}

func (s *Server) handleAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSONError(w, http.StatusMethodNotAllowed, "invalid_request", "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.cfg.PublicBaseURL,
		"authorization_endpoint":                s.cfg.PublicBaseURL + "/oauth/authorize",
		"token_endpoint":                        s.cfg.PublicBaseURL + "/oauth/token",
		"registration_endpoint":                 s.cfg.PublicBaseURL + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"code_challenge_methods_supported":      []string{"S256"},
		"scopes_supported":                      []string{defaultScope},
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}

	var req struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid JSON body")
		return
	}
	if len(req.RedirectURIs) == 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid_redirect_uri", "at least one redirect_uri is required")
		return
	}
	for _, rawURI := range req.RedirectURIs {
		if !isSafeRedirectURI(rawURI) {
			writeJSONError(w, http.StatusBadRequest, "invalid_redirect_uri", fmt.Sprintf("disallowed redirect_uri %q", rawURI))
			return
		}
	}

	client, err := s.store.RegisterOAuthClient(r.Context(), strings.TrimSpace(req.ClientName), req.RedirectURIs)
	if err != nil {
		s.logger.Error("register oauth client failed", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "server_error", "failed to register client")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  client.ClientID,
		"client_name":                client.ClientName,
		"redirect_uris":              client.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	clientID := strings.TrimSpace(q.Get("client_id"))
	redirectURI := strings.TrimSpace(q.Get("redirect_uri"))
	responseType := strings.TrimSpace(q.Get("response_type"))
	codeChallenge := strings.TrimSpace(q.Get("code_challenge"))
	codeChallengeMethod := strings.TrimSpace(q.Get("code_challenge_method"))
	state := q.Get("state")
	scope := strings.TrimSpace(q.Get("scope"))
	if scope == "" {
		scope = defaultScope
	}
	resource := strings.TrimSpace(q.Get("resource"))

	client, err := s.store.GetOAuthClient(r.Context(), clientID)
	if err != nil {
		http.Error(w, "Unknown or unregistered client_id", http.StatusBadRequest)
		return
	}
	if !containsString(client.RedirectURIs, redirectURI) {
		http.Error(w, "redirect_uri does not match registered client", http.StatusBadRequest)
		return
	}
	if responseType != "code" {
		redirectError(w, r, redirectURI, state, "unsupported_response_type", "response_type must be code")
		return
	}
	if codeChallenge == "" || codeChallengeMethod != "S256" {
		redirectError(w, r, redirectURI, state, "invalid_request", "PKCE S256 code_challenge is required")
		return
	}

	sess, err := s.store.CreateOAuthSession(r.Context(), storage.OAuthSession{
		ClientID:            client.ClientID,
		RedirectURI:         redirectURI,
		State:               state,
		Scope:               scope,
		CodeChallenge:       codeChallenge,
		CodeChallengeMethod: codeChallengeMethod,
		Resource:            resource,
	}, loginSessionTTL)
	if err != nil {
		s.logger.Error("create oauth session failed", "error", err)
		http.Error(w, "Failed to start authorization session", http.StatusInternalServerError)
		return
	}

	botUsername := s.store.GetPrimaryBotUsername(r.Context())
	deepLink := ""
	if botUsername != "" {
		deepLink = fmt.Sprintf("https://t.me/%s?start=login_%s", botUsername, sess.LoginNonce)
	}

	clientName := client.ClientName
	if clientName == "" {
		clientName = "MCP Client"
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")

	_ = authorizeTmpl.Execute(w, authorizePageData{
		ServerName:  s.cfg.MCPServerName,
		ClientName:  clientName,
		BotUsername: botUsername,
		DeepLink:    deepLink,
		Command:     "/start login_" + sess.LoginNonce,
		StatusURL:   "/oauth/status?nonce=" + url.QueryEscape(sess.LoginNonce),
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "invalid_request", "GET required")
		return
	}

	nonce := strings.TrimSpace(r.URL.Query().Get("nonce"))
	if nonce == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "nonce is required")
		return
	}

	sess, err := s.store.GetOAuthSession(r.Context(), nonce)
	if errors.Is(err, storage.ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "expired"})
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "server_error", "failed to check session")
		return
	}

	now := s.nowFunc().UTC()
	if now.Unix() >= sess.ExpiresAt && sess.Status == "pending" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "expired"})
		return
	}

	switch sess.Status {
	case "pending":
		writeJSON(w, http.StatusOK, map[string]string{"status": "pending"})
	case "denied":
		writeJSON(w, http.StatusOK, map[string]string{"status": "denied"})
	case "approved":
		rawCode, updated, err := s.store.IssueOAuthCodeForSession(r.Context(), nonce, now)
		if err != nil {
			// Already issued on a previous poll or expired.
			writeJSON(w, http.StatusOK, map[string]string{"status": "expired"})
			return
		}
		target, err := buildRedirectURL(updated.RedirectURI, map[string]string{
			"code":  rawCode,
			"state": updated.State,
		})
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "server_error", "invalid redirect_uri")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"status":       "approved",
			"redirect_url": target,
		})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "expired"})
	}
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}

	params, err := parseTokenParams(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	switch params["grant_type"] {
	case "authorization_code":
		s.handleAuthCodeGrant(w, r, params)
	case "refresh_token":
		s.handleRefreshTokenGrant(w, r, params)
	default:
		writeJSONError(w, http.StatusBadRequest, "unsupported_grant_type", "supported grant_types: authorization_code, refresh_token")
	}
}

func (s *Server) handleAuthCodeGrant(w http.ResponseWriter, r *http.Request, params map[string]string) {
	code := strings.TrimSpace(params["code"])
	clientID := strings.TrimSpace(params["client_id"])
	redirectURI := strings.TrimSpace(params["redirect_uri"])
	codeVerifier := strings.TrimSpace(params["code_verifier"])

	if code == "" || clientID == "" || redirectURI == "" || codeVerifier == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "code, client_id, redirect_uri, and code_verifier are required")
		return
	}

	sess, err := s.store.ConsumeOAuthCode(r.Context(), code, s.nowFunc())
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_grant", "invalid, expired, or already used authorization code")
		return
	}
	if sess.ClientID != clientID || sess.RedirectURI != redirectURI {
		writeJSONError(w, http.StatusBadRequest, "invalid_grant", "client_id or redirect_uri mismatch")
		return
	}
	if !verifyPKCES256(codeVerifier, sess.CodeChallenge) {
		writeJSONError(w, http.StatusBadRequest, "invalid_grant", "PKCE code_verifier verification failed")
		return
	}

	userID := *sess.TelegramUserID
	s.issueTokenResponse(w, r, clientID, userID, sess.Scope, "")
}

func (s *Server) handleRefreshTokenGrant(w http.ResponseWriter, r *http.Request, params map[string]string) {
	rawRefresh := strings.TrimSpace(params["refresh_token"])
	clientID := strings.TrimSpace(params["client_id"])
	if rawRefresh == "" || clientID == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "refresh_token and client_id are required")
		return
	}

	refreshTTL := time.Duration(s.cfg.OAuthRefreshTokenDays) * 24 * time.Hour
	newRefresh, rec, err := s.store.RotateOAuthRefreshToken(r.Context(), rawRefresh, clientID, refreshTTL, s.nowFunc())
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_grant", "invalid, revoked, or expired refresh_token")
		return
	}

	// Re-verify corporate chat membership on refresh so ex-employees cannot silently extend sessions.
	allowed := s.cfg.IsAdmin(rec.TelegramUserID)
	if !allowed && s.access != nil {
		ok, err := s.access.HasAnyTrackedChatAccess(r.Context(), rec.TelegramUserID)
		if err != nil {
			s.logger.Warn("oauth refresh membership check failed (fail-closed)", "user_id", rec.TelegramUserID, "error", err)
			writeJSONError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "failed to verify Telegram membership")
			return
		}
		allowed = ok
	}
	if !allowed {
		_, _ = s.store.RevokeUserTokens(r.Context(), rec.TelegramUserID)
		writeJSONError(w, http.StatusBadRequest, "invalid_grant", "user is no longer a member of any tracked workspace chat")
		return
	}

	s.issueTokenResponse(w, r, clientID, rec.TelegramUserID, rec.Scope, newRefresh)
}

func (s *Server) issueTokenResponse(w http.ResponseWriter, r *http.Request, clientID string, userID int64, scope, existingNewRefresh string) {
	accessToken, _, err := s.store.IssueAccessToken(r.Context(), userID, s.cfg.OAuthAccessTokenTTL)
	if err != nil {
		s.logger.Error("issue oauth access token failed", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "server_error", "failed to issue access token")
		return
	}

	refreshToken := existingNewRefresh
	if refreshToken == "" {
		refreshTTL := time.Duration(s.cfg.OAuthRefreshTokenDays) * 24 * time.Hour
		refreshToken, _, err = s.store.IssueOAuthRefreshToken(r.Context(), clientID, userID, scope, refreshTTL)
		if err != nil {
			s.logger.Error("issue oauth refresh token failed", "error", err)
			writeJSONError(w, http.StatusInternalServerError, "server_error", "failed to issue refresh token")
			return
		}
	}

	if scope == "" {
		scope = defaultScope
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  accessToken,
		"token_type":    "Bearer",
		"expires_in":    int(s.cfg.OAuthAccessTokenTTL.Seconds()),
		"refresh_token": refreshToken,
		"scope":         scope,
	})
}

func verifyPKCES256(verifier, expectedChallenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(expectedChallenge)) == 1
}

func isSafeRedirectURI(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "https":
		return u.Host != ""
	case "http":
		host := strings.ToLower(u.Hostname())
		return host == "localhost" || host == "127.0.0.1" || host == "::1"
	case "javascript", "data", "vbscript", "file":
		return false
	default:
		// Native desktop app custom URI schemes (e.g. cursor://, vscode://).
		return true
	}
}

func parseTokenParams(r *http.Request) (map[string]string, error) {
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	out := make(map[string]string)

	if strings.Contains(ct, "application/json") {
		var body map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			return nil, errors.New("invalid JSON token request")
		}
		for k, v := range body {
			if s, ok := v.(string); ok {
				out[k] = s
			}
		}
		return out, nil
	}

	if err := r.ParseForm(); err != nil {
		return nil, errors.New("invalid form-encoded token request")
	}
	for k, vals := range r.PostForm {
		if len(vals) > 0 {
			out[k] = vals[0]
		}
	}
	return out, nil
}

func buildRedirectURL(base string, params map[string]string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	q := u.Query()
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func redirectError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, desc string) {
	target, err := buildRedirectURL(redirectURI, map[string]string{
		"error":             code,
		"error_description": desc,
		"state":             state,
	})
	if err != nil {
		http.Error(w, desc, http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func containsString(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONError(w http.ResponseWriter, status int, errCode, desc string) {
	writeJSON(w, status, map[string]string{
		"error":             errCode,
		"error_description": desc,
	})
}

type authorizePageData struct {
	ServerName  string
	ClientName  string
	BotUsername string
	DeepLink    string
	Command     string
	StatusURL   string
}

var authorizeTmpl = template.Must(template.New("authorize").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Authorize {{.ServerName}} — mcpgram</title>
  <style>
    :root {
      --bg: #f8fafc; --card: #ffffff; --text: #0f172a; --muted: #475569;
      --border: #e2e8f0; --accent: #229ed9; --accent-hover: #1b8bc2;
      --ok: #15803d; --err: #b91c1c; --code-bg: #f1f5f9;
    }
    @media (prefers-color-scheme: dark) {
      :root {
        --bg: #0b1120; --card: #111827; --text: #f8fafc; --muted: #94a3b8;
        --border: #1f2937; --accent: #2aabee; --accent-hover: #229ed9;
        --ok: #4ade80; --err: #f87171; --code-bg: #1e293b;
      }
    }
    * { box-sizing: border-box; }
    body {
      margin: 0; min-height: 100vh; display: flex; align-items: center; justify-content: center;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      background: var(--bg); color: var(--text); padding: 20px;
    }
    .card {
      width: 100%; max-width: 460px; background: var(--card); border: 1px solid var(--border);
      border-radius: 16px; padding: 28px; box-shadow: 0 12px 32px rgba(0,0,0,0.08);
    }
    h1 { margin: 0 0 8px; font-size: 20px; }
    p { margin: 0 0 16px; color: var(--muted); font-size: 14px; line-height: 1.5; }
    .btn {
      display: block; width: 100%; text-align: center; text-decoration: none;
      background: var(--accent); color: #fff; font-weight: 600; font-size: 15px;
      padding: 13px 18px; border-radius: 10px; transition: background 0.15s;
    }
    .btn:hover { background: var(--accent-hover); }
    .code {
      background: var(--code-bg); border: 1px solid var(--border); border-radius: 8px;
      padding: 10px 12px; font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
      font-size: 12px; word-break: break-all; margin-top: 12px;
    }
    .status { margin-top: 18px; font-size: 13px; color: var(--muted); display: flex; align-items: center; gap: 8px; }
    .status.ok { color: var(--ok); font-weight: 600; }
    .status.err { color: var(--err); font-weight: 600; }
    .dot { width: 8px; height: 8px; border-radius: 50%; background: var(--accent); animation: pulse 1.2s infinite; }
    @keyframes pulse { 0%,100% { opacity: 0.3; } 50% { opacity: 1; } }
  </style>
</head>
<body>
  <div class="card">
    <h1>Authorize {{.ClientName}}</h1>
    <p>Sign in to <strong>{{.ServerName}}</strong> (Read-Only MCP access to workspace Telegram chats where you are a member).</p>
    {{if .DeepLink}}
      <a class="btn" href="{{.DeepLink}}" target="_blank" rel="noopener noreferrer">Confirm in Telegram (@{{.BotUsername}})</a>
      <p style="margin-top: 12px; margin-bottom: 4px; font-size: 12px;">Keep this tab open — it will redirect automatically once you press Start in Telegram. Or send this command directly to @{{.BotUsername}}:</p>
    {{else}}
      <p style="font-size: 13px;">Send this command to your workspace Telegram bot:</p>
    {{end}}
    <div class="code">{{.Command}}</div>
    <div id="status" class="status"><span class="dot"></span><span>Waiting for confirmation in Telegram...</span></div>
  </div>
  <script>
    (function poll() {
      fetch({{.StatusURL}}, { cache: "no-store" })
        .then(function(r) { return r.json(); })
        .then(function(data) {
          var el = document.getElementById("status");
          if (data.status === "approved" && data.redirect_url) {
            el.className = "status ok";
            el.textContent = "Approved! Redirecting back to your MCP client...";
            window.location.href = data.redirect_url;
            return;
          }
          if (data.status === "denied") {
            el.className = "status err";
            el.textContent = "Access denied: your Telegram account is not a member of any tracked workspace chat.";
            return;
          }
          if (data.status === "expired") {
            el.className = "status err";
            el.textContent = "Sign-in session expired. Please retry authorization from your MCP client.";
            return;
          }
          setTimeout(poll, 1500);
        })
        .catch(function() { setTimeout(poll, 2500); });
    })();
  </script>
</body>
</html>
`))
