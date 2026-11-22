package config

import (
	"testing"
	"time"
)

func mapLookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := LoadFromLookup(mapLookup(map[string]string{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DefaultHistoryPolicy != HistoryPolicyFull {
		t.Fatalf("expected default history policy %q, got %q", HistoryPolicyFull, cfg.DefaultHistoryPolicy)
	}
	if cfg.RetentionDays != 0 {
		t.Fatalf("expected RetentionDays=0 (unlimited by default), got %d", cfg.RetentionDays)
	}
	if cfg.TokenTTLDays != 90 {
		t.Fatalf("expected TokenTTLDays=90, got %d", cfg.TokenTTLDays)
	}
	if cfg.RBACCacheTTL != 5*time.Minute {
		t.Fatalf("expected RBACCacheTTL=5m, got %v", cfg.RBACCacheTTL)
	}
	if cfg.IgnoreTag != "#offrecord" {
		t.Fatalf("expected IgnoreTag=#offrecord, got %q", cfg.IgnoreTag)
	}
	if err := cfg.ValidateForServe(); err == nil {
		t.Fatal("expected ValidateForServe to fail without BOT_TOKEN")
	}
}

func TestLoadCustomValues(t *testing.T) {
	cfg, err := LoadFromLookup(mapLookup(map[string]string{
		"BOT_TOKEN":              "123456:ABC-token",
		"ADMIN_TG_IDS":           "101, 202, 101",
		"DATABASE_URL":           "sqlite:///data/mcpgram.db",
		"DEFAULT_HISTORY_POLICY": "since_join",
		"RETENTION_DAYS":         "30",
		"TOKEN_TTL_DAYS":         "14",
		"RBAC_CACHE_TTL":         "10m",
		"IGNORE_TAG":             "#private",
		"TELEGRAM_PROXY_URL":     "socks5://127.0.0.1:1080",
		"LOG_LEVEL":              "debug",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := cfg.ValidateForServe(); err != nil {
		t.Fatalf("expected ValidateForServe to pass, got %v", err)
	}
	if cfg.DatabasePath != "/data/mcpgram.db" {
		t.Fatalf("expected /data/mcpgram.db, got %q", cfg.DatabasePath)
	}
	if cfg.DefaultHistoryPolicy != HistoryPolicySinceJoin {
		t.Fatalf("expected since_join, got %q", cfg.DefaultHistoryPolicy)
	}
	if len(cfg.AdminTGIDs) != 2 || !cfg.IsAdmin(101) || !cfg.IsAdmin(202) || cfg.IsAdmin(999) {
		t.Fatalf("unexpected admin set: %v", cfg.AdminTGIDs)
	}
}

func TestLoadValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"invalid history policy", map[string]string{"DEFAULT_HISTORY_POLICY": "everything"}},
		{"invalid admin id", map[string]string{"ADMIN_TG_IDS": "abc"}},
		{"negative retention", map[string]string{"RETENTION_DAYS": "-1"}},
		{"negative token ttl", map[string]string{"TOKEN_TTL_DAYS": "-1"}},
		{"invalid rbac ttl", map[string]string{"RBAC_CACHE_TTL": "notaduration"}},
		{"invalid proxy scheme", map[string]string{"TELEGRAM_PROXY_URL": "ftp://proxy:21"}},
		{"invalid log level", map[string]string{"LOG_LEVEL": "trace"}},
		{"invalid digest mode", map[string]string{"DIGEST_MODE": "magic"}},
		{"digest missing base url", map[string]string{"DIGEST_MODE": "openai_compatible", "DIGEST_MODEL": "qwen2.5:7b"}},
		{"digest missing model", map[string]string{"DIGEST_MODE": "openai_compatible", "DIGEST_BASE_URL": "http://127.0.0.1:11434/v1"}},
		{"digest bad hour", map[string]string{"DIGEST_SCHEDULE_HOUR": "25"}},
		{"digest bad timezone", map[string]string{"DIGEST_TIMEZONE": "Mars/Olympus"}},
		{"invalid embedding mode", map[string]string{"EMBEDDING_MODE": "magic"}},
		{"embedding missing base url", map[string]string{"EMBEDDING_MODE": "openai_compatible", "EMBEDDING_MODEL": "bge-m3"}},
		{"embedding missing model", map[string]string{"EMBEDDING_MODE": "openai_compatible", "EMBEDDING_BASE_URL": "http://127.0.0.1:11434/v1"}},
		{"invalid whisper mode", map[string]string{"WHISPER_MODE": "magic"}},
		{"whisper missing base url", map[string]string{"WHISPER_MODE": "openai_compatible"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LoadFromLookup(mapLookup(tc.env)); err == nil {
				t.Fatalf("expected error for case %q", tc.name)
			}
		})
	}
}

func TestLoadDigestConfig(t *testing.T) {
	cfg, err := LoadFromLookup(mapLookup(map[string]string{
		"DIGEST_MODE":          "openai_compatible",
		"DIGEST_BASE_URL":      "http://127.0.0.1:11434/v1/",
		"DIGEST_MODEL":         "qwen2.5:7b",
		"DIGEST_SCHEDULE_HOUR": "4",
		"DIGEST_TIMEZONE":      "Europe/Moscow",
		"DIGEST_MIN_MESSAGES":  "5",
		"DIGEST_LOOKBACK_DAYS": "14",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DigestMode != DigestModeOpenAICompatible || cfg.DigestBaseURL != "http://127.0.0.1:11434/v1" {
		t.Fatalf("unexpected digest config: %+v", cfg)
	}
	if cfg.DigestScheduleHour != 4 || cfg.DigestTimezone != "Europe/Moscow" || cfg.DigestMinMessages != 5 || cfg.DigestLookbackDays != 14 {
		t.Fatalf("unexpected digest schedule: %+v", cfg)
	}
}
