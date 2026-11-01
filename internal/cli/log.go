package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/paranoidi/fileindex/scan"
)

// logHandler writes slog records as:
// "2026-10-15 12:00:00 - fileindex - WARNING - message".
// ponytail: one logger name for every package; add per-package names if logs need filtering.
type logHandler struct {
	mu    *sync.Mutex
	out   io.Writer
	level slog.Level
}

// logPrint emits a formatted log line; scan swaps it for Progress.Print.
// ponytail: global hook, fine for a one-command CLI process.
var logPrint = func(w io.Writer, b []byte) { _, _ = w.Write(b) }

func (h logHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h logHandler) Handle(_ context.Context, r slog.Record) error {
	name := map[slog.Level]string{slog.LevelDebug: "DEBUG", slog.LevelInfo: "INFO", slog.LevelWarn: "WARNING", slog.LevelError: "ERROR"}[r.Level]
	h.mu.Lock()
	defer h.mu.Unlock()
	logPrint(h.out, fmt.Appendf(nil, "%s - fileindex - %s - %s\n", r.Time.Format("2006-01-02 15:04:05"), name, r.Message))
	return nil
}

func (h logHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h logHandler) WithGroup(string) slog.Handler      { return h }

// setupLogging: DEBUG with -v, else output.log_level (unknown names mean INFO).
func setupLogging(out io.Writer, verbose bool, level string) {
	l := slog.LevelInfo
	switch strings.ToUpper(level) {
	case "DEBUG":
		l = slog.LevelDebug
	case "WARNING", "WARN":
		l = slog.LevelWarn
	case "ERROR", "CRITICAL", "FATAL":
		l = slog.LevelError
	}
	if verbose {
		l = slog.LevelDebug
	}
	scan.Logger = slog.New(logHandler{&sync.Mutex{}, out, l})
	slog.SetDefault(scan.Logger)
}
