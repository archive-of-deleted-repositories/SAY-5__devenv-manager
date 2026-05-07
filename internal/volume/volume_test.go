package volume

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/SAY-5/devenv-manager/internal/docker"
	"github.com/SAY-5/devenv-manager/pkg/api"
)

// fakeClient is a minimal docker.Client used by the volume tests.
type fakeClient struct {
	mu        sync.Mutex
	created   []string
	removed   []string
	createErr error
	removeErr error
}

func (f *fakeClient) Ping(ctx context.Context) (string, error) { return "test", nil }
func (f *fakeClient) CreateContainer(ctx context.Context, spec docker.ContainerSpec) (string, error) {
	return "", nil
}
func (f *fakeClient) StartContainer(ctx context.Context, id string) error { return nil }
func (f *fakeClient) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	return nil
}
func (f *fakeClient) RemoveContainer(ctx context.Context, id string, force bool) error { return nil }
func (f *fakeClient) InspectContainer(ctx context.Context, id string) (docker.ContainerInfo, error) {
	return docker.ContainerInfo{}, errors.New("not implemented")
}
func (f *fakeClient) ListLabeledContainers(ctx context.Context) ([]docker.ContainerInfo, error) {
	return nil, nil
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
func (f *fakeClient) CreateVolume(ctx context.Context, name string, labels map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	f.created = append(f.created, name)
	return name, nil
}
func (f *fakeClient) RemoveVolume(ctx context.Context, name string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, name)
	return nil
}
func (f *fakeClient) Close() error { return nil }

var _ io.Closer = (*fakeClient)(nil)

func newManager(t *testing.T, fc *fakeClient, now time.Time, retention time.Duration) *Manager {
	t.Helper()
	if fc == nil {
		fc = &fakeClient{}
	}
	return New(Config{
		Docker:    fc,
		Now:       func() time.Time { return now },
		Retention: retention,
	})
}

func TestCreateForSessionAddsTrackedVolume(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	fc := &fakeClient{}
	m := newManager(t, fc, now, time.Hour)

	v, err := m.CreateForSession(context.Background(), "session-1234abcd",
		api.VolumeSpec{Name: "data", MountPath: "/mnt/data"})
	if err != nil {
		t.Fatalf("CreateForSession: %v", err)
	}
	if v.ID == "" {
		t.Fatal("volume ID is empty")
	}
	if v.Status != api.VolumeStatusActive {
		t.Fatalf("status = %q", v.Status)
	}
	want := "devenv-session--data"
	if v.DockerVolumeID != want {
		t.Fatalf("docker name = %q, want %q", v.DockerVolumeID, want)
	}
	if !v.RetentionUntil.Equal(now.Add(time.Hour)) {
		t.Fatalf("retention = %v", v.RetentionUntil)
	}
	if len(fc.created) != 1 || fc.created[0] != want {
		t.Fatalf("docker created = %v", fc.created)
	}

	got, err := m.Get(v.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "data" {
		t.Fatalf("got.Name = %q", got.Name)
	}
}

func TestCreateValidatesInput(t *testing.T) {
	m := newManager(t, nil, time.Now(), time.Hour)
	if _, err := m.CreateForSession(context.Background(), "s", api.VolumeSpec{}); err == nil {
		t.Fatal("expected name validation error")
	}
	if _, err := m.CreateForSession(context.Background(), "s", api.VolumeSpec{Name: "data"}); err == nil {
		t.Fatal("expected mount_path validation error")
	}
}

func TestCreatePropagatesDockerError(t *testing.T) {
	fc := &fakeClient{createErr: errors.New("disk full")}
	m := newManager(t, fc, time.Now(), time.Hour)
	if _, err := m.CreateForSession(context.Background(), "s",
		api.VolumeSpec{Name: "data", MountPath: "/m"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestListFilterBySession(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	m := newManager(t, nil, now, time.Hour)
	v1, _ := m.CreateForSession(context.Background(), "session-A",
		api.VolumeSpec{Name: "data", MountPath: "/m"})
	_, _ = m.CreateForSession(context.Background(), "session-B",
		api.VolumeSpec{Name: "cache", MountPath: "/c"})

	allVols := m.List("")
	if len(allVols) != 2 {
		t.Fatalf("List(\"\") = %d", len(allVols))
	}
	a := m.List("session-A")
	if len(a) != 1 || a[0].ID != v1.ID {
		t.Fatalf("List(session-A) = %v", a)
	}
}

func TestMarkReleasedResetsRetention(t *testing.T) {
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clock := t0
	m := New(Config{
		Docker:    &fakeClient{},
		Now:       func() time.Time { return clock },
		Retention: 30 * time.Minute,
	})
	v, _ := m.CreateForSession(context.Background(), "session-A",
		api.VolumeSpec{Name: "data", MountPath: "/m"})
	if !v.RetentionUntil.Equal(t0.Add(30 * time.Minute)) {
		t.Fatalf("create retention = %v", v.RetentionUntil)
	}

	clock = t0.Add(15 * time.Minute)
	m.MarkReleased("session-A")
	got, _ := m.Get(v.ID)
	if got.Status != api.VolumeStatusReleased {
		t.Fatalf("status = %q", got.Status)
	}
	if !got.RetentionUntil.Equal(clock.Add(30 * time.Minute)) {
		t.Fatalf("released retention = %v want %v", got.RetentionUntil, clock.Add(30*time.Minute))
	}
}

func TestDeleteRemovesFromBothLayers(t *testing.T) {
	fc := &fakeClient{}
	m := newManager(t, fc, time.Now(), time.Hour)
	v, _ := m.CreateForSession(context.Background(), "session-A",
		api.VolumeSpec{Name: "data", MountPath: "/m"})
	if err := m.Delete(context.Background(), v.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := m.Get(v.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if len(fc.removed) != 1 {
		t.Fatalf("docker removed = %v", fc.removed)
	}
}

func TestDeleteUnknownReturnsErrNotFound(t *testing.T) {
	m := newManager(t, nil, time.Now(), time.Hour)
	if err := m.Delete(context.Background(), "unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestGCRemovesPastRetentionOnly(t *testing.T) {
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clock := t0
	fc := &fakeClient{}
	m := New(Config{
		Docker:    fc,
		Now:       func() time.Time { return clock },
		Retention: time.Minute,
	})
	expired, _ := m.CreateForSession(context.Background(), "session-A",
		api.VolumeSpec{Name: "old", MountPath: "/m1"})
	fresh, _ := m.CreateForSession(context.Background(), "session-A",
		api.VolumeSpec{Name: "fresh", MountPath: "/m2"})

	// Advance the clock and pin retention values so exactly one volume is
	// past its retention window.
	clock = t0.Add(2 * time.Minute)
	m.mu.Lock()
	m.volumes[expired.ID].RetentionUntil = t0
	m.volumes[fresh.ID].RetentionUntil = clock.Add(time.Hour)
	m.mu.Unlock()

	removed, errs := m.GC(context.Background())
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(removed) != 1 || removed[0] != expired.ID {
		t.Fatalf("removed = %v, want [%s]", removed, expired.ID)
	}
	got, _ := m.Get(expired.ID)
	if got.Status != api.VolumeStatusGCed {
		t.Fatalf("expected GCed status, got %q", got.Status)
	}
	if len(fc.removed) != 1 {
		t.Fatalf("docker removed = %v", fc.removed)
	}
}

func TestGCContinuesAfterError(t *testing.T) {
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clock := t0.Add(time.Hour)
	fc := &fakeClient{removeErr: errors.New("daemon timeout")}
	m := New(Config{
		Docker:    fc,
		Now:       func() time.Time { return clock },
		Retention: time.Minute,
	})
	_, _ = m.CreateForSession(context.Background(), "session-A",
		api.VolumeSpec{Name: "v1", MountPath: "/m"})
	_, _ = m.CreateForSession(context.Background(), "session-A",
		api.VolumeSpec{Name: "v2", MountPath: "/m"})

	// Force them all past retention.
	m.mu.Lock()
	for _, v := range m.volumes {
		v.RetentionUntil = t0
	}
	m.mu.Unlock()

	removed, errs := m.GC(context.Background())
	if len(removed) != 0 {
		t.Fatalf("expected 0 removed when docker fails, got %v", removed)
	}
	if len(errs) != 2 {
		t.Fatalf("expected 2 errors (one per volume), got %d", len(errs))
	}
}
