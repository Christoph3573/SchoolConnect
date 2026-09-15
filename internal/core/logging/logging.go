// Package logging stellt einen minimalen, stdlib-basierten Logger bereit.
package logging

import (
	"log/slog"
	"os"
)

// New erzeugt einen slog-Logger (Text-Handler auf stderr).
func New(level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	if level == "" {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
