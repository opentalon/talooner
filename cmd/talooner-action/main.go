// Command talooner-action is the GitHub Action entry point. It runs once per
// workflow event, inside the reviewed repo's own runner, and exits.
//
// Logs go to stderr, which is where the Actions runtime shows them. The exit
// code is the whole outcome: 0 for a run that did its job and for every
// deliberate skip, 1 for a run that broke.
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

	// A cancelled job gives the runner a SIGTERM before it kills the container;
	// honouring it stops mid-flight GitHub calls rather than leaving them to be
	// cut off.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(run.Main(ctx, log))
}

// level reads RUNNER_DEBUG, which Actions sets to "1" when a job is re-run with
// debug logging on.
func level() slog.Level {
	if os.Getenv("RUNNER_DEBUG") == "1" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}
