package digest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mytischi/mcpgram/internal/config"
	"github.com/mytischi/mcpgram/internal/storage"
)

// RunStats summarizes one batch of digest generation.
type RunStats struct {
	Generated int
	Skipped   int
	Failed    int
}

// Worker schedules and runs nightly chat digest generation.
type Worker struct {
	store      *storage.Store
	summarizer Summarizer
	cfg        config.Config
	loc        *time.Location
	logger     *slog.Logger
	nowFunc    func() time.Time
}

// NewWorker creates a digest worker. If cfg.DigestMode is off, summarizer may be nil.
func NewWorker(store *storage.Store, summarizer Summarizer, cfg config.Config, logger *slog.Logger) (*Worker, error) {
	if logger == nil {
		logger = slog.Default()
	}
	loc, err := time.LoadLocation(cfg.DigestTimezone)
	if err != nil {
		return nil, fmt.Errorf("load digest timezone %q: %w", cfg.DigestTimezone, err)
	}
	return &Worker{
		store:      store,
		summarizer: summarizer,
		cfg:        cfg,
		loc:        loc,
		logger:     logger,
		nowFunc:    time.Now,
	}, nil
}

// Enabled reports whether automatic/manual digest generation is configured.
func (w *Worker) Enabled() bool {
	return w.cfg.DigestMode == config.DigestModeOpenAICompatible && w.summarizer != nil
}

// Run sleeps until the configured nightly schedule hour in DIGEST_TIMEZONE and generates
// pending daily digests once per day until ctx is canceled.
func (w *Worker) Run(ctx context.Context) error {
	if !w.Enabled() {
		w.logger.Info("nightly digests disabled (DIGEST_MODE=off)")
		<-ctx.Done()
		return ctx.Err()
	}

	for {
		now := w.nowFunc().In(w.loc)
		nextRun := nextScheduleTime(now, w.cfg.DigestScheduleHour, w.loc)
		wait := time.Until(nextRun)
		if wait < 0 {
			wait = 0
		}

		w.logger.Info("nightly digest scheduled",
			"next_run", nextRun.Format(time.RFC3339),
			"timezone", w.loc.String(),
			"model", w.cfg.DigestModel,
		)

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}

		stats, err := w.GeneratePending(ctx)
		if err != nil {
			w.logger.Error("nightly digest run finished with error", "error", err)
		} else {
			w.logger.Info("nightly digest run finished",
				"generated", stats.Generated,
				"skipped", stats.Skipped,
				"failed", stats.Failed,
			)
		}
	}
}

// GeneratePending generates missing or stale digests across all tracked chats for the last
// DIGEST_LOOKBACK_DAYS completed calendar days (ending with yesterday in DIGEST_TIMEZONE).
func (w *Worker) GeneratePending(ctx context.Context) (RunStats, error) {
	if !w.Enabled() {
		return RunStats{}, errors.New("digest generation is disabled (set DIGEST_MODE=openai_compatible)")
	}

	chats, err := w.store.ListAllChats(ctx)
	if err != nil {
		return RunStats{}, fmt.Errorf("list chats for digest: %w", err)
	}

	nowLocal := w.nowFunc().In(w.loc)
	todayStart := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, w.loc)

	var stats RunStats
	for dayOffset := w.cfg.DigestLookbackDays; dayOffset >= 1; dayOffset-- {
		dayStart := todayStart.AddDate(0, 0, -dayOffset)
		dateStr := dayStart.Format("2006-01-02")

		for _, chat := range chats {
			if ctx.Err() != nil {
				return stats, ctx.Err()
			}
			generated, err := w.generateForChatDay(ctx, chat, dateStr, false)
			if err != nil {
				stats.Failed++
				w.logger.Error("generate digest failed",
					"workspace_chat_id", chat.WorkspaceChatID,
					"chat_title", chat.Title,
					"date", dateStr,
					"error", err,
				)
				continue
			}
			if generated {
				stats.Generated++
			} else {
				stats.Skipped++
			}
		}
	}

	return stats, nil
}

// GenerateAllForDate generates digests for all tracked chats (or a specific workspaceChatID if non-empty)
// on a specific calendar date (YYYY-MM-DD in DIGEST_TIMEZONE).
func (w *Worker) GenerateAllForDate(ctx context.Context, dateStr, workspaceChatID string, force bool) (RunStats, error) {
	if !w.Enabled() {
		return RunStats{}, errors.New("digest generation is disabled (set DIGEST_MODE=openai_compatible)")
	}
	if _, _, err := dayBounds(dateStr, w.loc); err != nil {
		return RunStats{}, err
	}

	var chats []storage.Chat
	if workspaceChatID != "" {
		c, err := w.store.GetChatByWorkspaceID(ctx, workspaceChatID)
		if err != nil {
			return RunStats{}, fmt.Errorf("load chat %s: %w", workspaceChatID, err)
		}
		chats = []storage.Chat{c}
	} else {
		var err error
		chats, err = w.store.ListAllChats(ctx)
		if err != nil {
			return RunStats{}, fmt.Errorf("list chats: %w", err)
		}
	}

	var stats RunStats
	for _, chat := range chats {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		generated, err := w.generateForChatDay(ctx, chat, dateStr, force)
		if err != nil {
			stats.Failed++
			w.logger.Error("generate digest failed",
				"workspace_chat_id", chat.WorkspaceChatID,
				"date", dateStr,
				"error", err,
			)
			continue
		}
		if generated {
			stats.Generated++
		} else {
			stats.Skipped++
		}
	}
	return stats, nil
}

// TodayDate returns today's YYYY-MM-DD string in DIGEST_TIMEZONE.
func (w *Worker) TodayDate() string {
	return w.nowFunc().In(w.loc).Format("2006-01-02")
}

// YesterdayDate returns yesterday's YYYY-MM-DD string in DIGEST_TIMEZONE.
func (w *Worker) YesterdayDate() string {
	now := w.nowFunc().In(w.loc)
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, w.loc)
	return todayStart.AddDate(0, 0, -1).Format("2006-01-02")
}

func (w *Worker) generateForChatDay(ctx context.Context, chat storage.Chat, dateStr string, force bool) (bool, error) {
	periodStart, periodEnd, err := dayBounds(dateStr, w.loc)
	if err != nil {
		return false, err
	}

	msgs, err := w.store.GetMessagesForPeriod(ctx, chat.WorkspaceChatID, periodStart, periodEnd)
	if err != nil {
		return false, err
	}
	if len(msgs) < w.cfg.DigestMinMessages {
		return false, nil
	}

	lastMsgID := msgs[len(msgs)-1].TelegramMessageID
	if !force {
		existing, err := w.store.GetDigest(ctx, chat.WorkspaceChatID, dateStr)
		if err == nil && existing.MessageCount == len(msgs) && existing.LastMessageID == lastMsgID {
			return false, nil
		}
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return false, err
		}
	}

	topicsMap, err := w.store.ListTopicsMap(ctx, chat.WorkspaceChatID)
	if err != nil {
		return false, err
	}

	transcript := formatTranscript(msgs, topicsMap, w.loc)
	if strings.TrimSpace(transcript) == "" {
		return false, nil
	}

	res, err := w.summarizer.SummarizeDay(ctx, chat.Title, dateStr, transcript)
	if err != nil {
		return false, err
	}

	if err := w.store.UpsertDigest(ctx, storage.DailyDigest{
		WorkspaceChatID: chat.WorkspaceChatID,
		DigestDate:      dateStr,
		PeriodStart:     periodStart,
		PeriodEnd:       periodEnd,
		MessageCount:    len(msgs),
		LastMessageID:   lastMsgID,
		Summary:         res.Summary,
		KeyDecisions:    res.KeyDecisions,
		ActionItems:     res.ActionItems,
		Topics:          res.Topics,
		Model:           res.Model,
	}); err != nil {
		return false, err
	}

	return true, nil
}

func dayBounds(dateStr string, loc *time.Location) (int64, int64, error) {
	day, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(dateStr), loc)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid digest date %q: expected YYYY-MM-DD", dateStr)
	}
	nextDay := day.AddDate(0, 0, 1)
	return day.Unix(), nextDay.Unix() - 1, nil
}

func nextScheduleTime(now time.Time, hour int, loc *time.Location) time.Time {
	n := now.In(loc)
	candidate := time.Date(n.Year(), n.Month(), n.Day(), hour, 0, 0, 0, loc)
	if !candidate.After(n) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate
}

func formatTranscript(msgs []storage.Message, topics map[int64]string, loc *time.Location) string {
	var b strings.Builder
	for _, m := range msgs {
		ts := time.Unix(m.CreatedAt, 0).In(loc).Format("15:04")
		author := m.SenderName
		if author == "" {
			if m.SenderUsername != "" {
				author = "@" + m.SenderUsername
			} else {
				author = "unknown"
			}
		}

		fmt.Fprintf(&b, "[%s #%d", ts, m.TelegramMessageID)
		if m.MessageThreadID != nil {
			if title, ok := topics[*m.MessageThreadID]; ok && title != "" {
				fmt.Fprintf(&b, " topic=%q", title)
			} else {
				fmt.Fprintf(&b, " topic=%d", *m.MessageThreadID)
			}
		}
		if m.ReplyToMessageID != nil {
			fmt.Fprintf(&b, " reply_to=#%d", *m.ReplyToMessageID)
		}
		fmt.Fprintf(&b, "] %s: ", author)

		text := strings.TrimSpace(m.Text)
		if text != "" {
			b.WriteString(strings.ReplaceAll(text, "\n", " "))
		}
		if m.AttachmentMeta != nil {
			if text != "" {
				b.WriteString(" ")
			}
			if m.AttachmentMeta.FileName != "" {
				fmt.Fprintf(&b, "[attachment:%s %s]", m.AttachmentMeta.Type, m.AttachmentMeta.FileName)
			} else {
				fmt.Fprintf(&b, "[attachment:%s]", m.AttachmentMeta.Type)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}
