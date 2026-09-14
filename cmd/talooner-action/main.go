package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/opentalon/talooner/internal/run"
	"github.com/opentalon/talooner/internal/version"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level()}))
	log.Info("talooner-action", "version", version.Version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(run.Main(ctx, log))
}

func level() slog.Level {
	if os.Getenv("RUNNER_DEBUG") == "1" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}
