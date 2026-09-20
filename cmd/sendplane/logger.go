package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
)

// newFlagSet returns a flag.FlagSet that reports errors instead of exiting
// the process, so run is testable.
func newFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("sendplane", flag.ContinueOnError)
	return fs
}

// newLogger builds the slog.Logger the whole process shares, per cfg.Level
// and cfg.Format.
func newLogger(cfg LogConfig, w io.Writer) (*slog.Logger, error) {
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	switch cfg.Format {
	case "text":
		handler = slog.NewTextHandler(w, opts)
	case "json", "":
		handler = slog.NewJSONHandler(w, opts)
	default:
		return nil, fmt.Errorf("unknown log.format %q", cfg.Format)
	}
	return slog.New(handler), nil
}

func parseLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log.level %q", s)
	}
}
