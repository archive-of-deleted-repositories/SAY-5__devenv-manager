// devenv-reaper: standalone one-shot reap utility. Runs the same decision
// logic as the in-server reaper but exits after a single pass. Useful for
// cron when the server is down for an extended period.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"time"

	"github.com/SAY-5/devenv-manager/internal/docker"
	"github.com/SAY-5/devenv-manager/internal/reaper"
)

func main() {
	timeout := flag.Duration("timeout", 30*time.Second, "max time per pass")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	dc, err := docker.New()
	if err != nil {
		logger.Error("docker: connect", "err", err)
		os.Exit(1)
	}
	defer func() { _ = dc.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	r := reaper.New(dc, reaper.Config{
		CurrentPID: os.Getpid(),
		Logger:     logger,
	})
	r.RunOnce(ctx)
	logger.Info("reaper: pass complete", "reaped", r.ReapedTotal())
}
