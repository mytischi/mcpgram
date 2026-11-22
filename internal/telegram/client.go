package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

const defaultBaseURL = "https://api.telegram.org"

// API defines the minimal Telegram Bot API surface required by mcpgram.
type API interface {
	GetMe(ctx context.Context) (User, error)
	SetMyCommands(ctx context.Context, commands []BotCommand) error
	GetUpdates(ctx context.Context, offset int64, timeoutSec int, allowedUpdates []string) ([]Update, error)
	GetChatMember(ctx context.Context, chatID, userID int64) (ChatMember, error)
	SendMessage(ctx context.Context, chatID int64, text string) error
}

// APIError represents an unsuccessful response returned by the Telegram Bot API.
type APIError struct {
	Method      string
	Code        int
	Description string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s failed (code %d): %s", e.Method, e.Code, e.Description)
}

// IsNotParticipantError reports whether err indicates that the queried user is not in the chat
// (distinct from transient network/rate-limit errors).
func IsNotParticipantError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.Code != http.StatusBadRequest {
		return false
	}
	desc := strings.ToLower(apiErr.Description)
	return strings.Contains(desc, "user not found") ||
		strings.Contains(desc, "member not found") ||
		strings.Contains(desc, "participant_id_invalid") ||
		strings.Contains(desc, "user_not_participant")
}

// Client is a lightweight Telegram Bot API HTTP client with optional HTTP/SOCKS5 proxy support.
type Client struct {
	token      string
	baseURL    string
	apiClient  *http.Client
	pollClient *http.Client
}

// ClientOption customizes Client construction (primarily for tests).
type ClientOption func(*Client)

// WithBaseURL overrides the Telegram API base URL.
func WithBaseURL(baseURL string) ClientOption {
	return func(c *Client) {
		c.baseURL = strings.TrimRight(baseURL, "/")
	}
}

// NewClient constructs a Telegram Bot API client. proxyURL may be empty, http(s)://, or socks5(h)://.
func NewClient(token, proxyURL string, opts ...ClientOption) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("telegram bot token must not be empty")
	}

	transport, err := buildTransport(proxyURL)
	if err != nil {
		return nil, err
	}

	c := &Client{
		token:   token,
		baseURL: defaultBaseURL,
		apiClient: &http.Client{
			Transport: transport,
			Timeout:   15 * time.Second,
		},
		pollClient: &http.Client{
			Transport: transport,
			Timeout:   60 * time.Second,
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

func buildTransport(proxyURL string) (*http.Transport, error) {
	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return tr, nil
	}

	u, err := url.Parse(proxyURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid telegram proxy url %q", proxyURL)
	}

	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		tr.Proxy = http.ProxyURL(u)
	case "socks5", "socks5h":
		dialer, err := proxy.FromURL(u, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("configure socks5 proxy: %w", err)
		}
		if ctxDialer, ok := dialer.(proxy.ContextDialer); ok {
			tr.DialContext = ctxDialer.DialContext
		} else {
			tr.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			}
		}
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}

	return tr, nil
}

func (c *Client) GetMe(ctx context.Context) (User, error) {
	var out User
	err := c.call(ctx, c.apiClient, "getMe", nil, &out)
	return out, err
}

func (c *Client) SetMyCommands(ctx context.Context, commands []BotCommand) error {
	payload := map[string]any{
		"commands": commands,
		"scope": map[string]string{
			"type": "all_private_chats",
		},
	}
	var ok bool
	return c.call(ctx, c.apiClient, "setMyCommands", payload, &ok)
}

func (c *Client) GetUpdates(ctx context.Context, offset int64, timeoutSec int, allowedUpdates []string) ([]Update, error) {
	payload := map[string]any{
		"offset":          offset,
		"timeout":         timeoutSec,
		"allowed_updates": allowedUpdates,
	}
	var out []Update
	err := c.call(ctx, c.pollClient, "getUpdates", payload, &out)
	return out, err
}

func (c *Client) GetChatMember(ctx context.Context, chatID, userID int64) (ChatMember, error) {
	payload := map[string]any{
		"chat_id": chatID,
		"user_id": userID,
	}
	var out ChatMember
	err := c.call(ctx, c.apiClient, "getChatMember", payload, &out)
	return out, err
}

func (c *Client) SendMessage(ctx context.Context, chatID int64, text string) error {
	payload := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	var out Message
	return c.call(ctx, c.apiClient, "sendMessage", payload, &out)
}

// GetFile resolves a Telegram file_id into a downloadable file_path.
func (c *Client) GetFile(ctx context.Context, fileID string) (File, error) {
	payload := map[string]any{
		"file_id": fileID,
	}
	var out File
	err := c.call(ctx, c.apiClient, "getFile", payload, &out)
	return out, err
}

// DownloadFile downloads a Telegram file strictly from the configured Bot API file endpoint,
// guarding against SSRF, path traversal, and oversized responses.
func (c *Client) DownloadFile(ctx context.Context, filePath string, maxBytes int64) ([]byte, error) {
	clean := strings.TrimSpace(filePath)
	if clean == "" || strings.HasPrefix(clean, "/") || strings.Contains(clean, "..") || strings.Contains(clean, "://") {
		return nil, fmt.Errorf("invalid telegram file_path %q", filePath)
	}
	if maxBytes <= 0 {
		maxBytes = 20 << 20 // 20 MiB
	}

	endpoint := fmt.Sprintf("%s/file/bot%s/%s", c.baseURL, c.token, clean)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, c.redactErr(fmt.Errorf("build download request: %w", err))
	}

	resp, err := c.pollClient.Do(req)
	if err != nil {
		return nil, c.redactErr(fmt.Errorf("download telegram file: %w", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download telegram file failed with http %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read telegram file: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("telegram file exceeds max allowed size (%d bytes)", maxBytes)
	}
	return data, nil
}

func (c *Client) call(ctx context.Context, httpClient *http.Client, method string, payload any, result any) error {
	endpoint := fmt.Sprintf("%s/bot%s/%s", c.baseURL, c.token, method)

	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal %s payload: %w", method, err)
		}
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return c.redactErr(fmt.Errorf("build %s request: %w", method, err))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return c.redactErr(fmt.Errorf("telegram %s request: %w", method, err))
	}
	defer resp.Body.Close()

	rawResp, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return fmt.Errorf("read %s response: %w", method, err)
	}

	var envelope APIResponse[json.RawMessage]
	if err := json.Unmarshal(rawResp, &envelope); err != nil {
		return fmt.Errorf("decode %s response (http %d): %w", method, resp.StatusCode, err)
	}

	if !envelope.OK {
		code := envelope.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		return &APIError{
			Method:      method,
			Code:        code,
			Description: envelope.Description,
		}
	}

	if result != nil && len(envelope.Result) > 0 {
		if err := json.Unmarshal(envelope.Result, result); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
	}
	return nil
}

// redactErr ensures the bot token can never leak into logs via net/url error strings.
func (c *Client) redactErr(err error) error {
	if err == nil || c.token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), c.token, "<redacted>"))
}
