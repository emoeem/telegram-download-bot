package archive

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

type LogFormat string

const (
	LogFormatText LogFormat = "text"
	LogFormatJSON LogFormat = "json"
)

func ParseLogLevel(v string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func NewStructuredLogger(w io.Writer, format LogFormat, level slog.Level) *slog.Logger {
	if w == nil {
		w = os.Stdout
	}
	opts := &slog.HandlerOptions{Level: level}
	if format == LogFormatJSON {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

func ConfigureLogging(format LogFormat, level slog.Level) *slog.Logger {
	logger := NewStructuredLogger(os.Stdout, format, level)
	slog.SetDefault(logger)
	return logger
}

func RedactSecret(v string) string {
	if strings.TrimSpace(v) == "" {
		return ""
	}
	return "[REDACTED]"
}

func valueOrZero(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func logMediaFailure(mediaID int64, kind, detail string, attempts int, nextRetryAt *time.Time) {
	attrs := []slog.Attr{
		slog.Int64("media_id", mediaID),
		slog.String("error_kind", kind),
		slog.String("error_detail", detail),
		slog.Int("attempts", attempts),
	}
	if nextRetryAt != nil {
		attrs = append(attrs, slog.Time("next_retry_at", *nextRetryAt))
	} else {
		attrs = append(attrs, slog.String("next_retry_at", ""))
	}
	slog.Default().LogAttrs(context.Background(), slog.LevelError, "media worker failed", attrs...)
}

func logJobFinished(logger *slog.Logger, started time.Time, status string, err error) {
	attrs := []slog.Attr{
		slog.String("status", status),
		slog.Int64("duration_ms", time.Since(started).Milliseconds()),
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	} else {
		attrs = append(attrs, slog.String("error", ""))
	}
	logger.LogAttrs(context.Background(), slog.LevelInfo, "archive job finished", attrs...)
}
