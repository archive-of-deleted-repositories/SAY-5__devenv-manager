package reaper

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/SAY-5/devenv-manager/internal/docker"
)

// fakeClient is a stub Docker client used to drive reaper decisions.
type fakeClient struct {
	mu       sync.Mutex
	listResp []docker.ContainerInfo
	stopped  []string
	removed  []string
	stopErr  error
	rmErr    error
}

func (f *fakeClient) Ping(ctx context.Context) (string, error) { return "test", nil }
func (f *fakeClient) CreateContainer(ctx context.Context, spec docker.ContainerSpec) (string, error) {
	return "", nil
}
func (f *fakeClient) StartContainer(ctx context.Context, id string) error { return nil }
func (f *fakeClient) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, id)
	return f.stopErr
}
func (f *fakeClient) RemoveContainer(ctx context.Context, id string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	return f.rmErr
}
func (f *fakeClient) InspectContainer(ctx context.Context, id string) (docker.ContainerInfo, error) {
	return docker.ContainerInfo{}, errors.New("not implemented")
}
func (f *fakeClient) ListLabeledContainers(ctx context.Context) ([]docker.ContainerInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]docker.ContainerInfo(nil), f.listResp...), nil
}
func (f *fakeClient) ExecAttach(ctx context.Context, id string, cmd []string, tty bool) (docker.HijackedConn, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeClient) ResizeExec(ctx context.Context, id string, cols, rows uint16) error { return nil }
func (f *fakeClient) Events(ctx context.Context, since time.Time) (<-chan docker.EventMessage, <-chan error) {
	out := make(chan docker.EventMessage)
	errs := make(chan error)
	close(out)
	close(errs)
	return out, errs
}
func (f *fakeClient) Close() error { return nil }

var _ io.Closer = (*fakeClient)(nil)

func TestEvaluateMatrix(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		info   docker.ContainerInfo
		alive  PIDChecker
		ourPID int
		want   Decision
	}{
		{
			name:   "no session id is left alone",
			info:   docker.ContainerInfo{ID: "x"},
			alive:  func(int) bool { return true },
			ourPID: 100,
			want:   DecisionKeep,
		},
		{
			name:   "lease expired is reaped",
			info:   docker.ContainerInfo{ID: "x", SessionID: "s", LeaseUntil: now.Add(-time.Second), CreatedByPID: 100},
			alive:  func(int) bool { return true },
			ourPID: 100,
			want:   DecisionReapTTL,
		},
		{
			name:   "lease in future and our pid is healthy",
			info:   docker.ContainerInfo{ID: "x", SessionID: "s", LeaseUntil: now.Add(time.Minute), CreatedByPID: 100},
			alive:  func(int) bool { return true },
			ourPID: 100,
			want:   DecisionKeep,
		},
		{
			name:   "different pid that is dead is orphan",
			info:   docker.ContainerInfo{ID: "x", SessionID: "s", LeaseUntil: now.Add(time.Minute), CreatedByPID: 999},
			alive:  func(int) bool { return false },
			ourPID: 100,
			want:   DecisionReapOrphan,
		},
		{
			name:   "different pid that is alive is kept",
			info:   docker.ContainerInfo{ID: "x", SessionID: "s", LeaseUntil: now.Add(time.Minute), CreatedByPID: 999},
			alive:  func(int) bool { return true },
			ourPID: 100,
			want:   DecisionKeep,
		},
		{
			name:   "lease expiry takes precedence over orphan",
			info:   docker.ContainerInfo{ID: "x", SessionID: "s", LeaseUntil: now.Add(-time.Second), CreatedByPID: 999},
			alive:  func(int) bool { return false },
			ourPID: 100,
			want:   DecisionReapTTL,
		},
		{
			name:   "lease zero (legacy) keeps when pid alive",
			info:   docker.ContainerInfo{ID: "x", SessionID: "s", LeaseUntil: time.Time{}, CreatedByPID: 100},
			alive:  func(int) bool { return true },
			ourPID: 100,
			want:   DecisionKeep,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := New(&fakeClient{}, Config{
				Now:        func() time.Time { return now },
				CurrentPID: tt.ourPID,
				PIDAlive:   tt.alive,
			})
			got := r.Evaluate(tt.info)
			if got != tt.want {
				t.Fatalf("Evaluate = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRunOnceReapsExpired(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	fc := &fakeClient{
		listResp: []docker.ContainerInfo{
			{ID: "expired", SessionID: "s1", LeaseUntil: now.Add(-time.Second), CreatedByPID: 100},
			{ID: "healthy", SessionID: "s2", LeaseUntil: now.Add(time.Minute), CreatedByPID: 100},
		},
	}
	r := New(fc, Config{
		Now:        func() time.Time { return now },
		CurrentPID: 100,
		PIDAlive:   func(int) bool { return true },
	})
	r.RunOnce(context.Background())

	if len(fc.stopped) != 1 || fc.stopped[0] != "expired" {
		t.Fatalf("stopped = %v", fc.stopped)
	}
	if len(fc.removed) != 1 || fc.removed[0] != "expired" {
		t.Fatalf("removed = %v", fc.removed)
	}
	if r.ReapedTotal() != 1 {
		t.Fatalf("ReapedTotal = %d", r.ReapedTotal())
	}
}

func TestRunOnceReapsOrphans(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	fc := &fakeClient{
		listResp: []docker.ContainerInfo{
			{ID: "orphan", SessionID: "s1", LeaseUntil: now.Add(time.Minute), CreatedByPID: 999},
		},
	}
	r := New(fc, Config{
		Now:        func() time.Time { return now },
		CurrentPID: 100,
		PIDAlive:   func(int) bool { return false },
	})
	r.RunOnce(context.Background())
	if len(fc.removed) != 1 || fc.removed[0] != "orphan" {
		t.Fatalf("removed = %v", fc.removed)
	}
}
