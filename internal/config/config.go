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

// DigestMode controls nightly chat digest generation.
type DigestMode string

const (
	DigestModeOff              DigestMode = "off"
	DigestModeOpenAICompatible DigestMode = "openai_compatible"
)

// Valid reports whether m is a supported DigestMode value.
func (m DigestMode) Valid() bool {
	return m == DigestModeOff || m == DigestModeOpenAICompatible
}

// EmbeddingMode controls local/OpenAI-compatible semantic search indexing.
type EmbeddingMode string

const (
	EmbeddingModeOff              EmbeddingMode = "off"
	EmbeddingModeOpenAICompatible EmbeddingMode = "openai_compatible"
)

// Valid reports whether m is a supported EmbeddingMode value.
func (m EmbeddingMode) Valid() bool {
	return m == EmbeddingModeOff || m == EmbeddingModeOpenAICompatible
}

// WhisperMode controls background voice & video_note transcription.
type WhisperMode string

const (
	WhisperModeOff              WhisperMode = "off"
	WhisperModeOpenAICompatible WhisperMode = "openai_compatible"
)

// Valid reports whether m is a supported WhisperMode value.
func (m WhisperMode) Valid() bool {
	return m == WhisperModeOff || m == WhisperModeOpenAICompatible
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

	// Nightly digests (v0.2)
	DigestMode         DigestMode
	DigestBaseURL      string
	DigestAPIKey       string
	DigestModel        string
	DigestLanguage     string
	DigestScheduleHour int
	DigestTimezone     string
	DigestMinMessages  int
	DigestLookbackDays int

	// Hybrid semantic search (v0.2)
	EmbeddingMode            EmbeddingMode
	EmbeddingBaseURL         string
	EmbeddingAPIKey          string
	EmbeddingModel           string
	EmbeddingChunkWindowSec  int
	EmbeddingChunkMaxMsgs    int
	EmbeddingPollInterval    time.Duration

	// Voice / video_note transcription (v0.2)
	WhisperMode         WhisperMode
	WhisperBaseURL      string
	WhisperAPIKey       string
	WhisperModel        string
	WhisperLanguage     string
	WhisperPollInterval time.Duration
	WhisperMaxFileBytes int64
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

	// Retention days (0 = keep forever, default; > 0 enables automatic TTL cleanup)
	retentionRaw := get("RETENTION_DAYS", "0")
	retention, err := strconv.Atoi(retentionRaw)
	if err != nil || retention < 0 {
		return Config{}, fmt.Errorf("invalid RETENTION_DAYS %q: must be >= 0", retentionRaw)
	}
	cfg.RetentionDays = retention

	// Token TTL days (0 = never expire until explicitly revoked; default 90)
	tokenTTLRaw := get("TOKEN_TTL_DAYS", "90")
	tokenTTL, err := strconv.Atoi(tokenTTLRaw)
	if err != nil || tokenTTL < 0 {
		return Config{}, fmt.Errorf("invalid TOKEN_TTL_DAYS %q: must be >= 0", tokenTTLRaw)
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

	// Nightly digest settings
	digestMode := DigestMode(strings.ToLower(get("DIGEST_MODE", string(DigestModeOff))))
	if !digestMode.Valid() {
		return Config{}, fmt.Errorf("invalid DIGEST_MODE %q: must be %q or %q",
			digestMode, DigestModeOff, DigestModeOpenAICompatible)
	}
	cfg.DigestMode = digestMode
	cfg.DigestBaseURL = strings.TrimRight(get("DIGEST_BASE_URL", ""), "/")
	cfg.DigestAPIKey = get("DIGEST_API_KEY", "")
	cfg.DigestModel = get("DIGEST_MODEL", "")
	cfg.DigestLanguage = get("DIGEST_LANGUAGE", "")

	scheduleHourRaw := get("DIGEST_SCHEDULE_HOUR", "3")
	scheduleHour, err := strconv.Atoi(scheduleHourRaw)
	if err != nil || scheduleHour < 0 || scheduleHour > 23 {
		return Config{}, fmt.Errorf("invalid DIGEST_SCHEDULE_HOUR %q: must be 0..23", scheduleHourRaw)
	}
	cfg.DigestScheduleHour = scheduleHour

	cfg.DigestTimezone = get("DIGEST_TIMEZONE", "UTC")
	if _, err := time.LoadLocation(cfg.DigestTimezone); err != nil {
		return Config{}, fmt.Errorf("invalid DIGEST_TIMEZONE %q: %w", cfg.DigestTimezone, err)
	}

	minMsgsRaw := get("DIGEST_MIN_MESSAGES", "3")
	minMsgs, err := strconv.Atoi(minMsgsRaw)
	if err != nil || minMsgs < 1 {
		return Config{}, fmt.Errorf("invalid DIGEST_MIN_MESSAGES %q: must be >= 1", minMsgsRaw)
	}
	cfg.DigestMinMessages = minMsgs

	lookbackRaw := get("DIGEST_LOOKBACK_DAYS", "7")
	lookback, err := strconv.Atoi(lookbackRaw)
	if err != nil || lookback < 1 {
		return Config{}, fmt.Errorf("invalid DIGEST_LOOKBACK_DAYS %q: must be >= 1", lookbackRaw)
	}
	cfg.DigestLookbackDays = lookback

	if cfg.DigestMode == DigestModeOpenAICompatible {
		if cfg.DigestBaseURL == "" {
			return Config{}, errors.New("DIGEST_BASE_URL is required when DIGEST_MODE=openai_compatible")
		}
		u, err := url.Parse(cfg.DigestBaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return Config{}, fmt.Errorf("invalid DIGEST_BASE_URL %q: must be http(s) URL", cfg.DigestBaseURL)
		}
		if cfg.DigestModel == "" {
			return Config{}, errors.New("DIGEST_MODEL is required when DIGEST_MODE=openai_compatible")
		}
	}

	// Hybrid semantic search settings
	embMode := EmbeddingMode(strings.ToLower(get("EMBEDDING_MODE", string(EmbeddingModeOff))))
	if !embMode.Valid() {
		return Config{}, fmt.Errorf("invalid EMBEDDING_MODE %q: must be %q or %q",
			embMode, EmbeddingModeOff, EmbeddingModeOpenAICompatible)
	}
	cfg.EmbeddingMode = embMode
	cfg.EmbeddingBaseURL = strings.TrimRight(get("EMBEDDING_BASE_URL", ""), "/")
	cfg.EmbeddingAPIKey = get("EMBEDDING_API_KEY", "")
	cfg.EmbeddingModel = get("EMBEDDING_MODEL", "")

	chunkWinRaw := get("EMBEDDING_CHUNK_WINDOW_SEC", "600")
	chunkWin, err := strconv.Atoi(chunkWinRaw)
	if err != nil || chunkWin <= 0 {
		return Config{}, fmt.Errorf("invalid EMBEDDING_CHUNK_WINDOW_SEC %q: must be > 0", chunkWinRaw)
	}
	cfg.EmbeddingChunkWindowSec = chunkWin

	chunkMaxRaw := get("EMBEDDING_CHUNK_MAX_MESSAGES", "8")
	chunkMax, err := strconv.Atoi(chunkMaxRaw)
	if err != nil || chunkMax <= 0 {
		return Config{}, fmt.Errorf("invalid EMBEDDING_CHUNK_MAX_MESSAGES %q: must be > 0", chunkMaxRaw)
	}
	cfg.EmbeddingChunkMaxMsgs = chunkMax

	embPollRaw := get("EMBEDDING_POLL_INTERVAL", "2m")
	embPoll, err := time.ParseDuration(embPollRaw)
	if err != nil || embPoll <= 0 {
		return Config{}, fmt.Errorf("invalid EMBEDDING_POLL_INTERVAL %q: must be positive Go duration", embPollRaw)
	}
	cfg.EmbeddingPollInterval = embPoll

	if cfg.EmbeddingMode == EmbeddingModeOpenAICompatible {
		if cfg.EmbeddingBaseURL == "" {
			return Config{}, errors.New("EMBEDDING_BASE_URL is required when EMBEDDING_MODE=openai_compatible")
		}
		u, err := url.Parse(cfg.EmbeddingBaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return Config{}, fmt.Errorf("invalid EMBEDDING_BASE_URL %q: must be http(s) URL", cfg.EmbeddingBaseURL)
		}
		if cfg.EmbeddingModel == "" {
			return Config{}, errors.New("EMBEDDING_MODEL is required when EMBEDDING_MODE=openai_compatible")
		}
	}

	// Whisper transcription settings
	whisperMode := WhisperMode(strings.ToLower(get("WHISPER_MODE", string(WhisperModeOff))))
	if !whisperMode.Valid() {
		return Config{}, fmt.Errorf("invalid WHISPER_MODE %q: must be %q or %q",
			whisperMode, WhisperModeOff, WhisperModeOpenAICompatible)
	}
	cfg.WhisperMode = whisperMode
	cfg.WhisperBaseURL = strings.TrimRight(get("WHISPER_BASE_URL", ""), "/")
	cfg.WhisperAPIKey = get("WHISPER_API_KEY", "")
	cfg.WhisperModel = get("WHISPER_MODEL", "whisper-1")
	cfg.WhisperLanguage = get("WHISPER_LANGUAGE", "")

	whisperPollRaw := get("WHISPER_POLL_INTERVAL", "30s")
	whisperPoll, err := time.ParseDuration(whisperPollRaw)
	if err != nil || whisperPoll <= 0 {
		return Config{}, fmt.Errorf("invalid WHISPER_POLL_INTERVAL %q: must be positive Go duration", whisperPollRaw)
	}
	cfg.WhisperPollInterval = whisperPoll

	whisperMaxRaw := get("WHISPER_MAX_FILE_BYTES", "20971520") // 20 MiB (Bot API getFile limit)
	whisperMax, err := strconv.ParseInt(whisperMaxRaw, 10, 64)
	if err != nil || whisperMax <= 0 {
		return Config{}, fmt.Errorf("invalid WHISPER_MAX_FILE_BYTES %q: must be > 0", whisperMaxRaw)
	}
	cfg.WhisperMaxFileBytes = whisperMax

	if cfg.WhisperMode == WhisperModeOpenAICompatible {
		if cfg.WhisperBaseURL == "" {
			return Config{}, errors.New("WHISPER_BASE_URL is required when WHISPER_MODE=openai_compatible")
		}
		u, err := url.Parse(cfg.WhisperBaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return Config{}, fmt.Errorf("invalid WHISPER_BASE_URL %q: must be http(s) URL", cfg.WhisperBaseURL)
		}
		if cfg.WhisperModel == "" {
			return Config{}, errors.New("WHISPER_MODEL is required when WHISPER_MODE=openai_compatible")
		}
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
