// devenv-manager server: HTTP + WebSocket API for Docker-backed dev sessions.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/SAY-5/devenv-manager/internal/api"
	"github.com/SAY-5/devenv-manager/internal/docker"
	"github.com/SAY-5/devenv-manager/internal/reaper"
	"github.com/SAY-5/devenv-manager/internal/session"
	"github.com/SAY-5/devenv-manager/internal/volume"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	defaultTTL := flag.Duration("default-ttl", 60*time.Second, "default session TTL")
	maxTTL := flag.Duration("max-ttl", 30*time.Minute, "maximum session TTL")
	reapInterval := flag.Duration("reap-interval", 30*time.Second, "reaper loop interval")
	owner := flag.String("owner", "anonymous", "owner label applied to containers")
	volumeRetention := flag.Duration("volume-retention", volume.DefaultRetention,
		"how long a session's named volumes survive after the session is reaped")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	dc, err := docker.New()
	if err != nil {
		logger.Error("docker: connect", "err", err)
		os.Exit(1)
	}
	defer func() { _ = dc.Close() }()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if _, err := dc.Ping(ctx); err != nil {
		logger.Error("docker: ping", "err", err)
		os.Exit(1)
	}

	mgr := session.New(time.Now)
	volMgr := volume.New(volume.Config{
		Docker:    dc,
		Now:       time.Now,
		Retention: *volumeRetention,
	})

	// Hydrate from existing labeled containers — this makes restarts idempotent
	// and keeps the session table addressable across crashes.
	if infos, herr := dc.ListLabeledContainers(ctx); herr == nil {
		mgr.HydrateFromContainers(infos)
		logger.Info("session: hydrated", "count", len(infos))
	} else {
		logger.Warn("session: hydrate", "err", herr)
	}

	r := reaper.New(dc, reaper.Config{
		Interval:   *reapInterval,
		CurrentPID: os.Getpid(),
		Logger:     logger,
	})
	go r.Run(ctx)

	registry := prometheus.NewRegistry()
	for _, c := range r.Collectors() {
		registry.MustRegister(c)
	}
	registry.MustRegister(collectors.NewGoCollector())

	dropped := &atomic.Int64{}

	srv := api.NewServer(api.Config{
		Docker:     dc,
		Sessions:   mgr,
		Volumes:    volMgr,
		DefaultTTL: *defaultTTL,
		MaxTTL:     *maxTTL,
		Owner:      *owner,
		Logger:     logger,
		PID:        os.Getpid(),
		DroppedOut: dropped,
	})

	mux := http.NewServeMux()
	mux.Handle("/", srv.Handler())
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	logger.Info("server: listening", "addr", *addr, "pid", os.Getpid())
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server: serve", "err", err)
		os.Exit(1)
	}
}
