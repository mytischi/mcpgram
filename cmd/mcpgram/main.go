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
	_ "time/tzdata"

	"github.com/mytischi/mcpgram/internal/config"
	"github.com/mytischi/mcpgram/internal/digest"
	"github.com/mytischi/mcpgram/internal/importer"
	"github.com/mytischi/mcpgram/internal/mcpserver"
	"github.com/mytischi/mcpgram/internal/oauth"
	"github.com/mytischi/mcpgram/internal/rbac"
	"github.com/mytischi/mcpgram/internal/retention"
	"github.com/mytischi/mcpgram/internal/semantic"
	"github.com/mytischi/mcpgram/internal/storage"
	"github.com/mytischi/mcpgram/internal/telegram"
	"github.com/mytischi/mcpgram/internal/whisper"
)

const version = "0.3.0"

type digestTriggerAdapter struct {
	w *digest.Worker
}

func (a digestTriggerAdapter) Enabled() bool { return a.w.Enabled() }

func (a digestTriggerAdapter) RunPending(ctx context.Context) (int, int, int, error) {
	st, err := a.w.GeneratePending(ctx)
	return st.Generated, st.Skipped, st.Failed, err
}

func (a digestTriggerAdapter) RunForDate(ctx context.Context, dateStr string, force bool) (int, int, int, error) {
	st, err := a.w.GenerateAllForDate(ctx, dateStr, "", force)
	return st.Generated, st.Skipped, st.Failed, err
}

func (a digestTriggerAdapter) TodayDate() string { return a.w.TodayDate() }

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
	case "digest":
		return runDigest(subArgs)
	case "reindex":
		return runReindex(subArgs)
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
  mcpgram serve                         Run Telegram ingestor + Streamable HTTP MCP server + optional workers (digests, semantic, whisper)
  mcpgram import --file result.json     Import historical chat archive from Telegram Desktop JSON export
  mcpgram digest [--date YYYY-MM-DD]    Generate daily chat digests on demand (requires DIGEST_MODE=openai_compatible)
  mcpgram reindex                       Build/update semantic dialog chunk embeddings (requires EMBEDDING_MODE=openai_compatible)
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

func newDigestWorker(store *storage.Store, cfg config.Config, logger *slog.Logger) (*digest.Worker, error) {
	var summarizer digest.Summarizer
	if cfg.DigestMode == config.DigestModeOpenAICompatible {
		summarizer = digest.NewOpenAIClient(cfg.DigestBaseURL, cfg.DigestAPIKey, cfg.DigestModel, cfg.DigestLanguage)
	}
	return digest.NewWorker(store, summarizer, cfg, logger)
}

func newEmbedder(cfg config.Config) semantic.Embedder {
	if cfg.EmbeddingMode == config.EmbeddingModeOpenAICompatible {
		return semantic.NewOpenAIEmbedder(cfg.EmbeddingBaseURL, cfg.EmbeddingAPIKey, cfg.EmbeddingModel)
	}
	return nil
}

func newWhisperWorker(store *storage.Store, fetcher whisper.FileFetcher, cfg config.Config, logger *slog.Logger) *whisper.Worker {
	var tr whisper.Transcriber
	if cfg.WhisperMode == config.WhisperModeOpenAICompatible {
		tr = whisper.NewOpenAIClient(cfg.WhisperBaseURL, cfg.WhisperAPIKey, cfg.WhisperModel, cfg.WhisperLanguage)
	}
	return whisper.NewWorker(store, fetcher, tr, cfg, logger)
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

	digestWorker, err := newDigestWorker(store, cfg, logger)
	if err != nil {
		return fmt.Errorf("create digest worker: %w", err)
	}

	embedder := newEmbedder(cfg)
	semanticIndexer := semantic.NewIndexer(store, embedder, cfg, logger)
	searcher := semantic.NewHybridSearcher(store, embedder, logger)
	whisperWorker := newWhisperWorker(store, tgClient, cfg, logger)

	authorizer := rbac.NewAuthorizer(store, tgClient, cfg.RBACCacheTTL, logger)
	ingestor := telegram.NewIngestor(tgClient, store, cfg, authorizer, logger)
	ingestor.SetDigestRunner(digestTriggerAdapter{w: digestWorker})

	retentionWorker := retention.NewWorker(store, cfg.RetentionDays, time.Hour, logger)
	mcpSrv := mcpserver.New(store, searcher, authorizer, cfg, logger)

	if cfg.OAuthEnabled {
		oauthSrv := oauth.NewServer(store, cfg, authorizer, logger)
		mcpSrv.SetOAuth(oauthSrv)
		ingestor.SetOAuthApprover(oauthSrv)
	}

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mcpSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 6)

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
		if err := digestWorker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- fmt.Errorf("digest worker stopped: %w", err)
		}
	}()

	go func() {
		if err := semanticIndexer.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- fmt.Errorf("semantic indexer stopped: %w", err)
		}
	}()

	go func() {
		if err := whisperWorker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- fmt.Errorf("whisper worker stopped: %w", err)
		}
	}()

	go func() {
		logger.Info("starting Streamable HTTP MCP server",
			"addr", cfg.ListenAddr,
			"endpoint", "/mcp",
			"default_history_policy", cfg.DefaultHistoryPolicy,
			"retention_days", cfg.RetentionDays,
			"digest_mode", cfg.DigestMode,
			"embedding_mode", cfg.EmbeddingMode,
			"whisper_mode", cfg.WhisperMode,
			"oauth_enabled", cfg.OAuthEnabled,
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
	filePath := fs.String("file", "", "Path to Telegram export file: JSON (result.json) or HTML (messages.html) (required)")
	chatID := fs.Int64("chat-id", 0, "Optional Bot API telegram_chat_id override (e.g. -1001234567890)")
	dbPath := fs.String("db", cfg.DatabasePath, "SQLite database path")
	ignoreTag := fs.String("ignore-tag", cfg.IgnoreTag, "Messages containing this tag are skipped")
	retentionDays := fs.Int("retention-days", cfg.RetentionDays, "Skip messages older than N days (0 = import all)")
	timezone := fs.String("tz", cfg.DigestTimezone, "IANA timezone for HTML export timestamps without UTC offset (e.g. Europe/Moscow)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *filePath == "" {
		return errors.New("--file is required (e.g. mcpgram import --file result.json or messages.html)")
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
		Timezone:               *timezone,
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

func runDigest(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	fs := flag.NewFlagSet("digest", flag.ContinueOnError)
	dateStr := fs.String("date", "", "Generate digests for a specific calendar date YYYY-MM-DD (default: pending lookback days)")
	today := fs.Bool("today", false, "Generate digests for today's messages so far")
	chatID := fs.String("chat-id", "", "Optional workspace_chat_id to restrict generation to a single chat")
	force := fs.Bool("force", false, "Regenerate even if an up-to-date digest already exists")
	dbPath := fs.String("db", cfg.DatabasePath, "SQLite database path")

	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := newLogger(cfg.LogLevel)
	ctx := context.Background()
	store, err := storage.Open(ctx, *dbPath)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer store.Close()

	worker, err := newDigestWorker(store, cfg, logger)
	if err != nil {
		return err
	}

	var stats digest.RunStats
	targetDate := *dateStr
	if *today {
		targetDate = worker.TodayDate()
	}

	if targetDate != "" {
		stats, err = worker.GenerateAllForDate(ctx, targetDate, *chatID, *force)
	} else {
		stats, err = worker.GeneratePending(ctx)
	}
	if err != nil {
		return err
	}

	fmt.Printf("Digest run complete: generated=%d skipped=%d failed=%d\n",
		stats.Generated, stats.Skipped, stats.Failed)
	return nil
}

func runReindex(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	fs := flag.NewFlagSet("reindex", flag.ContinueOnError)
	dbPath := fs.String("db", cfg.DatabasePath, "SQLite database path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := newLogger(cfg.LogLevel)
	ctx := context.Background()
	store, err := storage.Open(ctx, *dbPath)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer store.Close()

	indexer := semantic.NewIndexer(store, newEmbedder(cfg), cfg, logger)
	stats, err := indexer.IndexAll(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("Semantic reindex complete: chats=%d indexed=%d skipped=%d failed=%d\n",
		stats.ChatsScanned, stats.Indexed, stats.Skipped, stats.Failed)
	return nil
}
