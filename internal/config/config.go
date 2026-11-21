package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// HistoryPolicy controls whether chat members can read messages sent before they joined.
type HistoryPolicy string

const (
	HistoryPolicyFull      HistoryPolicy = "full_history"
	HistoryPolicySinceJoin HistoryPolicy = "since_join"
)

// Valid reports whether p is a supported HistoryPolicy value.
func (p HistoryPolicy) Valid() bool {
	return p == HistoryPolicyFull || p == HistoryPolicySinceJoin
}

// Config holds validated runtime configuration for mcpgram.
type Config struct {
	BotToken             string
	AdminTGIDs           []int64
	adminSet             map[int64]struct{}
	ListenAddr           string
	DatabasePath         string
	DefaultHistoryPolicy HistoryPolicy
	RetentionDays        int
	TokenTTLDays         int
	RBACCacheTTL         time.Duration
	IgnoreTag            string
	TelegramProxyURL     string
	LogLevel             string
}

// Load reads optional .env files (if they exist) and parses environment variables.
func Load(envFiles ...string) (Config, error) {
	if len(envFiles) == 0 {
		envFiles = []string{".env"}
	}
	for _, f := range envFiles {
		if f == "" {
			continue
		}
		if _, err := os.Stat(f); err == nil {
			if err := godotenv.Load(f); err != nil {
				return Config{}, fmt.Errorf("load env file %q: %w", f, err)
			}
		}
	}

	return LoadFromLookup(os.LookupEnv)
}

// LoadFromLookup parses configuration using a custom environment lookup function.
func LoadFromLookup(lookup func(string) (string, bool)) (Config, error) {
	get := func(key, fallback string) string {
		if v, ok := lookup(key); ok {
			return strings.TrimSpace(v)
		}
		return fallback
	}

	cfg := Config{
		BotToken:         get("BOT_TOKEN", ""),
		ListenAddr:       get("LISTEN_ADDR", ":8080"),
		DatabasePath:     normalizeDatabasePath(get("DATABASE_URL", "./data/mcpgram.db")),
		IgnoreTag:        get("IGNORE_TAG", "#offrecord"),
		TelegramProxyURL: get("TELEGRAM_PROXY_URL", ""),
		LogLevel:         strings.ToLower(get("LOG_LEVEL", "info")),
		adminSet:         make(map[int64]struct{}),
	}

	// Admin Telegram IDs
	if rawAdmins := get("ADMIN_TG_IDS", ""); rawAdmins != "" {
		for _, part := range strings.Split(rawAdmins, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			id, err := strconv.ParseInt(part, 10, 64)
			if err != nil || id <= 0 {
				return Config{}, fmt.Errorf("invalid ADMIN_TG_IDS entry %q: must be positive int64", part)
			}
			if _, exists := cfg.adminSet[id]; !exists {
				cfg.adminSet[id] = struct{}{}
				cfg.AdminTGIDs = append(cfg.AdminTGIDs, id)
			}
		}
	}

	// Default history policy
	policy := HistoryPolicy(strings.ToLower(get("DEFAULT_HISTORY_POLICY", string(HistoryPolicyFull))))
	if !policy.Valid() {
		return Config{}, fmt.Errorf("invalid DEFAULT_HISTORY_POLICY %q: must be %q or %q",
			policy, HistoryPolicyFull, HistoryPolicySinceJoin)
	}
	cfg.DefaultHistoryPolicy = policy

	// Retention days (0 disables automatic deletion)
	retentionRaw := get("RETENTION_DAYS", "90")
	retention, err := strconv.Atoi(retentionRaw)
	if err != nil || retention < 0 {
		return Config{}, fmt.Errorf("invalid RETENTION_DAYS %q: must be >= 0", retentionRaw)
	}
	cfg.RetentionDays = retention

	// Token TTL days
	tokenTTLRaw := get("TOKEN_TTL_DAYS", "90")
	tokenTTL, err := strconv.Atoi(tokenTTLRaw)
	if err != nil || tokenTTL <= 0 {
		return Config{}, fmt.Errorf("invalid TOKEN_TTL_DAYS %q: must be > 0", tokenTTLRaw)
	}
	cfg.TokenTTLDays = tokenTTL

	// RBAC cache TTL
	rbacTTLRaw := get("RBAC_CACHE_TTL", "5m")
	rbacTTL, err := time.ParseDuration(rbacTTLRaw)
	if err != nil || rbacTTL <= 0 {
		return Config{}, fmt.Errorf("invalid RBAC_CACHE_TTL %q: must be positive Go duration", rbacTTLRaw)
	}
	cfg.RBACCacheTTL = rbacTTL

	// Proxy URL validation
	if cfg.TelegramProxyURL != "" {
		u, err := url.Parse(cfg.TelegramProxyURL)
		if err != nil || u.Host == "" {
			return Config{}, fmt.Errorf("invalid TELEGRAM_PROXY_URL %q", cfg.TelegramProxyURL)
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "socks5", "socks5h":
		default:
			return Config{}, fmt.Errorf("unsupported TELEGRAM_PROXY_URL scheme %q", u.Scheme)
		}
	}

	// Log level validation
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return Config{}, fmt.Errorf("invalid LOG_LEVEL %q: must be debug, info, warn, or error", cfg.LogLevel)
	}

	if cfg.DatabasePath == "" {
		return Config{}, errors.New("DATABASE_URL must not be empty")
	}

	return cfg, nil
}

// ValidateForServe ensures settings required to run the live bot + MCP server are present.
func (c Config) ValidateForServe() error {
	if strings.TrimSpace(c.BotToken) == "" {
		return errors.New("BOT_TOKEN is required to run the server")
	}
	return nil
}

// IsAdmin reports whether userID is configured in ADMIN_TG_IDS.
func (c Config) IsAdmin(userID int64) bool {
	_, ok := c.adminSet[userID]
	return ok
}

func normalizeDatabasePath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if raw == ":memory:" || raw == "sqlite://:memory:" || raw == "sqlite:///:memory:" {
		return ":memory:"
	}
	if strings.HasPrefix(raw, "sqlite:///") {
		return "/" + strings.TrimPrefix(raw, "sqlite:///")
	}
	if strings.HasPrefix(raw, "sqlite://") {
		return strings.TrimPrefix(raw, "sqlite://")
	}
	return raw
}
