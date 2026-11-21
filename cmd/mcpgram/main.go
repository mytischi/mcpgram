package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mytischi/mcpgram/internal/config"
	"github.com/mytischi/mcpgram/internal/importer"
	"github.com/mytischi/mcpgram/internal/mcpserver"
	"github.com/mytischi/mcpgram/internal/rbac"
	"github.com/mytischi/mcpgram/internal/retention"
	"github.com/mytischi/mcpgram/internal/storage"
	"github.com/mytischi/mcpgram/internal/telegram"
)

const version = "0.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "mcpgram: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "serve"
	var subArgs []string
	if len(args) > 0 {
		cmd = args[0]
		subArgs = args[1:]
	}

	switch cmd {
	case "serve":
		return runServe()
	case "import":
		return runImport(subArgs)
	case "version", "--version", "-v":
		fmt.Printf("mcpgram v%s\n", version)
		return nil
	case "help", "--help", "-h":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func printUsage() {
	fmt.Println(`mcpgram - Self-hosted Telegram Corporate MCP Server

Usage:
  mcpgram serve                         Run Telegram Long Polling ingestor + Streamable HTTP MCP server
  mcpgram import --file result.json     Import historical chat archive from Telegram Desktop JSON export
  mcpgram version                       Print version`)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

func runServe() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.ValidateForServe(); err != nil {
		return err
	}

	logger := newLogger(cfg.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := storage.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer store.Close()

	tgClient, err := telegram.NewClient(cfg.BotToken, cfg.TelegramProxyURL)
	if err != nil {
		return fmt.Errorf("create telegram client: %w", err)
	}

	authorizer := rbac.NewAuthorizer(store, tgClient, cfg.RBACCacheTTL, logger)
	ingestor := telegram.NewIngestor(tgClient, store, cfg, authorizer, logger)
	retentionWorker := retention.NewWorker(store, cfg.RetentionDays, time.Hour, logger)
	mcpSrv := mcpserver.New(store, store, authorizer, cfg, logger)

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mcpSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 3)

	go func() {
		logger.Info("starting Telegram Long Polling ingestor")
		if err := ingestor.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- fmt.Errorf("ingestor stopped: %w", err)
		}
	}()

	go func() {
		if err := retentionWorker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- fmt.Errorf("retention worker stopped: %w", err)
		}
	}()

	go func() {
		logger.Info("starting Streamable HTTP MCP server",
			"addr", cfg.ListenAddr,
			"endpoint", "/mcp",
			"default_history_policy", cfg.DefaultHistoryPolicy,
			"retention_days", cfg.RetentionDays,
		)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http server stopped: %w", err)
		}
	}()

	var runErr error
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errCh:
		runErr = err
		stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http server shutdown error", "error", err)
	}

	return runErr
}

func runImport(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	filePath := fs.String("file", "", "Path to Telegram Desktop result.json export file (required)")
	chatID := fs.Int64("chat-id", 0, "Optional Bot API telegram_chat_id override (e.g. -1001234567890)")
	dbPath := fs.String("db", cfg.DatabasePath, "SQLite database path")
	ignoreTag := fs.String("ignore-tag", cfg.IgnoreTag, "Messages containing this tag are skipped")
	retentionDays := fs.Int("retention-days", cfg.RetentionDays, "Skip messages older than N days (0 = import all)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *filePath == "" {
		return errors.New("--file is required (e.g. mcpgram import --file result.json)")
	}

	ctx := context.Background()
	store, err := storage.Open(ctx, *dbPath)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer store.Close()

	stats, err := importer.ImportDesktopFile(ctx, store, *filePath, importer.Options{
		TelegramChatIDOverride: *chatID,
		IgnoreTag:              *ignoreTag,
		RetentionDays:          *retentionDays,
	})
	if err != nil {
		return err
	}

	fmt.Printf("Import complete:\n"+
		"  Chat: %s (telegram_chat_id=%d, workspace_chat_id=%s)\n"+
		"  Imported: %d\n"+
		"  Ignored (%s): %d\n"+
		"  Skipped (older than retention): %d\n"+
		"  Skipped (service/empty): %d\n",
		stats.ChatTitle, stats.TelegramChatID, stats.WorkspaceChatID,
		stats.Imported, *ignoreTag, stats.IgnoredOffrec,
		stats.SkippedOld, stats.SkippedNonMsg,
	)
	return nil
}
