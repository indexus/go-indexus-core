// Package logging configures the process-wide logger.
package logging

import (
	"log"
	"log/slog"
	"os"
	"strings"
)

// Setup installs the default logger for a binary. INDEXUS_LOG_LEVEL picks the
// verbosity (debug, info, warn, error) and INDEXUS_LOG_FORMAT picks text for a
// terminal or json for a log collector. Setting slog's default also routes the
// standard log package through the same handler, so third-party libraries and
// any leftover log.Print end up in one stream.
func Setup(component string) {
	options := &slog.HandlerOptions{Level: level()}

	var handler slog.Handler = slog.NewTextHandler(os.Stderr, options)
	if strings.EqualFold(os.Getenv("INDEXUS_LOG_FORMAT"), "json") {
		handler = slog.NewJSONHandler(os.Stderr, options)
	}

	slog.SetDefault(slog.New(handler).With("component", component))
}

// StdLogger adapts the default logger for the standard library APIs that still
// want a *log.Logger, http.Server being the one that matters here.
func StdLogger(level slog.Level) *log.Logger {
	return slog.NewLogLogger(slog.Default().Handler(), level)
}

func level() slog.Level {
	var parsed slog.Level
	if err := parsed.UnmarshalText([]byte(os.Getenv("INDEXUS_LOG_LEVEL"))); err != nil {
		return slog.LevelInfo
	}
	return parsed
}
