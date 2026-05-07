// Package reaper implements the idle-cleanup loop. Every Interval, it
// inspects all containers carrying com.devenv.id labels and decides
// whether each is healthy, lease-expired, or orphaned by a dead creator
// process. Expired and orphaned containers are stopped and removed.
package reaper

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/SAY-5/devenv-manager/internal/docker"
	"github.com/prometheus/client_golang/prometheus"
)

// PIDChecker reports whether a process with the given PID is alive.
// Defaulted to a real check on the host but kept injectable for tests.
type PIDChecker func(pid int) bool

// Reaper periodically reaps containers whose lease has lapsed.
type Reaper struct {
	docker     docker.Client
	interval   time.Duration
	stopGrace  time.Duration
	now        func() time.Time
	currentPID int
	pidAlive   PIDChecker
	logger     *slog.Logger

	reapedTTL    prometheus.Counter
	reapedOrphan prometheus.Counter
	reapErrors   prometheus.Counter

	reapedTotal int64
}

// Config is the reaper configuration.
type Config struct {
	Interval   time.Duration
	StopGrace  time.Duration
	Now        func() time.Time
	CurrentPID int
	PIDAlive   PIDChecker
	Logger     *slog.Logger
}

// New constructs a Reaper.
func New(d docker.Client, cfg Config) *Reaper {
	if cfg.Interval == 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.StopGrace == 0 {
		cfg.StopGrace = 5 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.CurrentPID == 0 {
		cfg.CurrentPID = os.Getpid()
	}
	if cfg.PIDAlive == nil {
		cfg.PIDAlive = ProcessAlive
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Reaper{
		docker:     d,
		interval:   cfg.Interval,
		stopGrace:  cfg.StopGrace,
		now:        cfg.Now,
		currentPID: cfg.CurrentPID,
		pidAlive:   cfg.PIDAlive,
		logger:     cfg.Logger,
		reapedTTL: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "devenv_reaped_total_ttl",
			Help: "Containers reaped because their lease expired.",
		}),
		reapedOrphan: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "devenv_reaped_total_orphan",
			Help: "Containers reaped because the creator PID is gone.",
		}),
		reapErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "devenv_reap_errors_total",
			Help: "Errors encountered while reaping.",
		}),
	}
}

// Collectors returns the Prometheus collectors so the server can register them.
func (r *Reaper) Collectors() []prometheus.Collector {
	return []prometheus.Collector{r.reapedTTL, r.reapedOrphan, r.reapErrors}
}

// Run loops every Interval until ctx is done.
func (r *Reaper) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	// Run once immediately so service restarts reap orphans without waiting.
	r.RunOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.RunOnce(ctx)
		}
	}
}

// Decision is the outcome of evaluating a single container.
type Decision int

const (
	// DecisionKeep means the container is healthy and should remain.
	DecisionKeep Decision = iota
	// DecisionReapTTL means the lease has expired.
	DecisionReapTTL
	// DecisionReapOrphan means the creator PID is gone.
	DecisionReapOrphan
)

// String returns a stable label suitable for metrics.
func (d Decision) String() string {
	switch d {
	case DecisionReapTTL:
		return "ttl"
	case DecisionReapOrphan:
		return "orphan"
	default:
		return "keep"
	}
}

// Evaluate returns the Decision for a single container without performing
// any side effects. Exposed for testing the decision matrix.
func (r *Reaper) Evaluate(info docker.ContainerInfo) Decision {
	if info.SessionID == "" {
		return DecisionKeep
	}
	if !info.LeaseUntil.IsZero() && info.LeaseUntil.Before(r.now()) {
		return DecisionReapTTL
	}
	if info.CreatedByPID > 0 && info.CreatedByPID != r.currentPID && !r.pidAlive(info.CreatedByPID) {
		return DecisionReapOrphan
	}
	return DecisionKeep
}

// RunOnce performs a single reap pass. Exposed so the standalone CLI can
// drive the same code path.
func (r *Reaper) RunOnce(ctx context.Context) {
	infos, err := r.docker.ListLabeledContainers(ctx)
	if err != nil {
		r.reapErrors.Inc()
		r.logger.Error("reaper: list", "err", err)
		return
	}
	for _, info := range infos {
		switch r.Evaluate(info) {
		case DecisionReapTTL:
			r.reap(ctx, info, "ttl")
			r.reapedTTL.Inc()
			atomic.AddInt64(&r.reapedTotal, 1)
		case DecisionReapOrphan:
			r.reap(ctx, info, "orphan")
			r.reapedOrphan.Inc()
			atomic.AddInt64(&r.reapedTotal, 1)
		}
	}
}

// ReapedTotal returns the cumulative reap count. Useful for the chaos test.
func (r *Reaper) ReapedTotal() int64 {
	return atomic.LoadInt64(&r.reapedTotal)
}

func (r *Reaper) reap(ctx context.Context, info docker.ContainerInfo, reason string) {
	r.logger.Info("reaper: reaping", "session", info.SessionID, "container", info.ID, "reason", reason)
	if err := r.docker.StopContainer(ctx, info.ID, r.stopGrace); err != nil {
		r.reapErrors.Inc()
		r.logger.Warn("reaper: stop failed", "container", info.ID, "err", err)
	}
	if err := r.docker.RemoveContainer(ctx, info.ID, true); err != nil {
		r.reapErrors.Inc()
		r.logger.Warn("reaper: remove failed", "container", info.ID, "err", err)
	}
}
