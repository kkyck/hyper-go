package main

// logger.go — Structured logging with slog (Go 1.21+).
// Lazy evaluation — only formats if level enabled.

import (
	"log/slog"
	"os"
)

var (
	logger     *slog.Logger
	loggerInit bool
)

func initLogger() {
	if loggerInit {
		return
	}
	loggerInit = true
	logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func logDebug(msg string, args ...any) {
	if logger == nil {
		initLogger()
	}
	logger.Debug(msg, args...)
}
func logInfo(msg string, args ...any) {
	if logger == nil {
		initLogger()
	}
	logger.Info(msg, args...)
}
func logWarn(msg string, args ...any) {
	if logger == nil {
		initLogger()
	}
	logger.Warn(msg, args...)
}
func logError(msg string, args ...any) {
	if logger == nil {
		initLogger()
	}
	logger.Error(msg, args...)
}
