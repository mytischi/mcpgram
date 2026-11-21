package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/mytischi/mcpgram/internal/config"
	"github.com/mytischi/mcpgram/internal/storage"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "mcpgram: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}

	switch cmd {
	case "serve":
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		if err := cfg.ValidateForServe(); err != nil {
			return err
		}

		ctx := context.Background()
		store, err := storage.Open(ctx, cfg.DatabasePath)
		if err != nil {
			return fmt.Errorf("open storage: %w", err)
		}
		defer store.Close()

		slog.Info("mcpgram initialized",
			"db", cfg.DatabasePath,
			"listen", cfg.ListenAddr,
			"default_history_policy", cfg.DefaultHistoryPolicy,
			"retention_days", cfg.RetentionDays,
		)
		return fmt.Errorf("serve runtime (Stages 2-4: Telegram ingestor, RBAC, MCP server) not implemented yet")

	case "import":
		return fmt.Errorf("import command (Stage 5: Telegram Desktop result.json importer) not implemented yet")

	case "version", "--version", "-v":
		fmt.Println("mcpgram v0.1.0-dev")
		return nil

	default:
		return fmt.Errorf("unknown command %q (expected: serve, import, version)", cmd)
	}
}
