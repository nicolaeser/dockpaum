package log

import (
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/nicolaeser/dockpaum/internal/config"
)

func New(cfg config.LogConfig) *slog.Logger { return NewTo(os.Stdout, cfg) }

func NewTo(w io.Writer, cfg config.LogConfig) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if IsText(cfg) {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h).With("app", "dockpaum")
}

func IsText(cfg config.LogConfig) bool { return strings.EqualFold(cfg.Format, "text") }
