package oauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mytischi/mcpgram/internal/config"
	"github.com/mytischi/mcpgram/internal/storage"
)

type fakeAccess struct {
	allowed map[int64]bool
}

func (f fakeAccess) HasAnyTrackedChatAccess(_ context.Context, userID int64) (bool, error) {
	return f.allowed[userID], nil
}

func TestOAuth21FullPKCEAndRefreshFlow(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	_ = store.UpsertBot(ctx, storage.Bot{BotID: 1, Username: "corp_mcp_bot"})

	cfg, err := config.LoadFromLookup(func(k string) (string, bool) {
		m := map[string]string{
			"OAUTH_ENABLED":            "true",
			"PUBLIC_BASE_URL":          "https://tg-mcp.example.com",
			"MCP_SERVER_NAME":          "company-telegram",
			"OAUTH_ACCESS_TOKEN_TTL":   "1h",
			"OAUTH_REFRESH_TOKEN_DAYS": "30",
		}
		v, ok := m[k]
		return v, ok
	})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	access := fakeAccess{allowed: map[int64]bool{777: true}}
	srv := NewServer(store, cfg, access, nil)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// 1. Discovery endpoints (RFC 9728 + RFC 8414).
	resp, err := http.Get(ts.URL + "/.well-known/oauth-protected-resource")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("protected resource discovery failed: (%v, %v)", resp, err)
	}
	_ = resp.Body.Close()

	resp, err = http.Get(ts.URL + "/.well-known/oauth-authorization-server")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("auth server discovery failed: (%v, %v)", resp, err)
	}
	_ = resp.Body.Close()

	// 2. Dynamic Client Registration (RFC 7591).
	regBody := `{"client_name":"Cursor","redirect_uris":["http://127.0.0.1:54321/callback"]}`
	resp, err = http.Post(ts.URL+"/oauth/register", "application/json", strings.NewReader(regBody))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("register client failed: (%v, %v)", resp, err)
	}
	var regOut struct {
		ClientID string `json:"client_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&regOut)
	_ = resp.Body.Close()
	if regOut.ClientID == "" {
		t.Fatal("expected non-empty client_id")
	}

	// 3. Start PKCE S256 authorization flow.
	verifier := "abcdefghijklmnopqrstuvwxyz0123456789-ABCDEFGHIJKLMNO"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	authURL := ts.URL + "/oauth/authorize?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {regOut.ClientID},
		"redirect_uri":          {"http://127.0.0.1:54321/callback"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"xyz123"},
	}.Encode()
	resp, err = http.Get(authURL)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("authorize page failed: (%v, %v)", resp, err)
	}
	var htmlBuf bytes.Buffer
	_, _ = htmlBuf.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	page := htmlBuf.String()
	if !strings.Contains(page, "https://t.me/corp_mcp_bot?start=login_") {
		t.Fatalf("expected Telegram deep-link in authorize page, got: %s", page)
	}

	// Extract nonce from deep-link.
	idx := strings.Index(page, "start=login_")
	rest := page[idx+len("start=login_"):]
	nonce := rest[:strings.Index(rest, `"`)]

	// 4. Approve session via Telegram bot handler.
	if err := srv.ApproveLogin(ctx, nonce, 777); err != nil {
		t.Fatalf("ApproveLogin failed: %v", err)
	}

	// 5. Browser polls /oauth/status -> receives one-time authorization code in redirect_url.
	resp, err = http.Get(ts.URL + "/oauth/status?nonce=" + url.QueryEscape(nonce))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("status poll failed: (%v, %v)", resp, err)
	}
	var statusOut struct {
		Status      string `json:"status"`
		RedirectURL string `json:"redirect_url"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&statusOut)
	_ = resp.Body.Close()
	if statusOut.Status != "approved" || statusOut.RedirectURL == "" {
		t.Fatalf("expected approved redirect_url, got %+v", statusOut)
	}
	redirParsed, _ := url.Parse(statusOut.RedirectURL)
	authCode := redirParsed.Query().Get("code")
	if authCode == "" || redirParsed.Query().Get("state") != "xyz123" {
		t.Fatalf("unexpected redirect query: %s", statusOut.RedirectURL)
	}

	// 6. Exchange code + PKCE verifier for access_token + refresh_token.
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"client_id":     {regOut.ClientID},
		"redirect_uri":  {"http://127.0.0.1:54321/callback"},
		"code_verifier": {verifier},
	}
	resp, err = http.PostForm(ts.URL+"/oauth/token", form)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("token exchange failed: (%v, %v)", resp, err)
	}
	var tokOut struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&tokOut)
	_ = resp.Body.Close()
	if tokOut.AccessToken == "" || tokOut.RefreshToken == "" || tokOut.TokenType != "Bearer" {
		t.Fatalf("unexpected token response: %+v", tokOut)
	}

	// Access token validates against auth_tokens for user 777!
	uid, err := store.ValidateAuthToken(ctx, tokOut.AccessToken, time.Now())
	if err != nil || uid != 777 {
		t.Fatalf("expected valid OAuth access_token for 777, got (%d, %v)", uid, err)
	}

	// 7. Refresh token rotates cleanly.
	refreshForm := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {tokOut.RefreshToken},
		"client_id":     {regOut.ClientID},
	}
	resp, err = http.PostForm(ts.URL+"/oauth/token", refreshForm)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh token failed: (%v, %v)", resp, err)
	}
	var refreshed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&refreshed)
	_ = resp.Body.Close()
	if refreshed.RefreshToken == "" || refreshed.RefreshToken == tokOut.RefreshToken {
		t.Fatalf("expected rotated refresh token, got %+v", refreshed)
	}

	// 8. /revoke_user revokes BOTH access tokens and refresh tokens immediately.
	if _, err := store.RevokeUserTokens(ctx, 777); err != nil {
		t.Fatalf("revoke user tokens: %v", err)
	}
	if _, err := store.ValidateAuthToken(ctx, refreshed.AccessToken, time.Now()); err == nil {
		t.Fatal("expected revoked access token to fail")
	}
	resp, _ = http.PostForm(ts.URL+"/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshed.RefreshToken},
		"client_id":     {regOut.ClientID},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 invalid_grant after revocation, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
}
