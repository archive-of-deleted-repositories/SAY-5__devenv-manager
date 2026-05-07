package imagebuild

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SAY-5/devenv-manager/internal/docker"
	"github.com/SAY-5/devenv-manager/pkg/api"
)

func writeFile(p, body string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(body), 0o600)
}

// fakeDocker is a docker.Client used by the imagebuild tests. The build
// stream is configurable: callers set buildStream to a sequence of
// engine-API frames (NDJSON) to drive the Manager's drain loop.
type fakeDocker struct {
	mu          sync.Mutex
	buildStream string
	buildErr    error
	buildSpecs  []docker.ImageBuildSpec
}

func (f *fakeDocker) Ping(ctx context.Context) (string, error) { return "test", nil }
func (f *fakeDocker) CreateContainer(ctx context.Context, spec docker.ContainerSpec) (string, error) {
	return "", nil
}
func (f *fakeDocker) StartContainer(ctx context.Context, id string) error { return nil }
func (f *fakeDocker) StopContainer(ctx context.Context, id string, t time.Duration) error {
	return nil
}
func (f *fakeDocker) RemoveContainer(ctx context.Context, id string, force bool) error { return nil }
func (f *fakeDocker) InspectContainer(ctx context.Context, id string) (docker.ContainerInfo, error) {
	return docker.ContainerInfo{ID: id, State: "running"}, nil
}
func (f *fakeDocker) ListLabeledContainers(ctx context.Context) ([]docker.ContainerInfo, error) {
	return nil, nil
}
func (f *fakeDocker) ExecAttach(ctx context.Context, id string, cmd []string, tty bool) (docker.HijackedConn, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeDocker) ResizeExec(ctx context.Context, id string, c, r uint16) error { return nil }
func (f *fakeDocker) Events(ctx context.Context, since time.Time) (<-chan docker.EventMessage, <-chan error) {
	out := make(chan docker.EventMessage)
	errs := make(chan error)
	close(out)
	close(errs)
	return out, errs
}
func (f *fakeDocker) CreateVolume(ctx context.Context, name string, labels map[string]string) (string, error) {
	return name, nil
}
func (f *fakeDocker) RemoveVolume(ctx context.Context, name string, force bool) error { return nil }
func (f *fakeDocker) BuildImage(ctx context.Context, spec docker.ImageBuildSpec) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.buildErr != nil {
		return nil, f.buildErr
	}
	f.buildSpecs = append(f.buildSpecs, spec)
	return io.NopCloser(strings.NewReader(f.buildStream)), nil
}
func (f *fakeDocker) Close() error { return nil }

// helper to build NDJSON frames the way the engine emits them.
func ndjson(frames ...map[string]any) string {
	var sb strings.Builder
	for _, f := range frames {
		b, _ := json.Marshal(f)
		sb.Write(b)
		sb.WriteByte('\n')
	}
	return sb.String()
}

func newTestManager(t *testing.T, fd *fakeDocker, reg Registry) *Manager {
	t.Helper()
	if reg == nil {
		reg = NewMemoryRegistry(map[string]string{
			"node22-typescript": "FROM scratch\n",
			"python312-poetry":  "FROM scratch\n",
			"rust-stable":       "FROM scratch\n",
		})
	}
	m, err := New(Config{
		Docker:       fd,
		Templates:    reg,
		LogsDir:      t.TempDir(),
		MaxWallClock: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func TestSubmitRejectsUnknownTemplate(t *testing.T) {
	m := newTestManager(t, &fakeDocker{buildStream: ""}, nil)
	_, err := m.Submit(context.Background(), "alice",
		api.BuildImageRequest{Template: "evil-template"})
	if err == nil {
		t.Fatal("expected rejection")
	}
}

func TestSubmitHappyPath(t *testing.T) {
	fd := &fakeDocker{buildStream: ndjson(
		map[string]any{"stream": "Step 1/2 : FROM scratch\n"},
		map[string]any{"stream": "Successfully built abcdef\n"},
	)}
	m := newTestManager(t, fd, nil)

	resp, err := m.Submit(context.Background(), "alice",
		api.BuildImageRequest{Template: "node22-typescript"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if resp.Status != api.ImageBuildStatusQueued {
		t.Fatalf("initial status = %q", resp.Status)
	}

	// Wait until the goroutine flips the status to ready.
	deadline := time.Now().Add(2 * time.Second)
	var got api.BuildImageResponse
	for time.Now().Before(deadline) {
		got, _ = m.Get(resp.ID)
		if got.Status == api.ImageBuildStatusReady {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.Status != api.ImageBuildStatusReady {
		t.Fatalf("final status = %q (err=%q)", got.Status, got.ErrorMessage)
	}

	// The owner allowlist should now include the built tag.
	allowed := m.AllowedImagesFor("alice")
	if len(allowed) != 1 || !strings.HasPrefix(allowed[0], "devenv-builds/node22-typescript:") {
		t.Fatalf("allowed = %v", allowed)
	}

	// Logs should contain the streamed lines.
	logBytes, _ := m.Log(resp.ID)
	if !strings.Contains(string(logBytes), "FROM scratch") {
		t.Fatalf("log missing build output: %q", logBytes)
	}
}

func TestSubmitErrorFrameFails(t *testing.T) {
	fd := &fakeDocker{buildStream: ndjson(
		map[string]any{"stream": "Step 1/2\n"},
		map[string]any{"error": "exit code 1"},
	)}
	m := newTestManager(t, fd, nil)

	resp, _ := m.Submit(context.Background(), "alice",
		api.BuildImageRequest{Template: "node22-typescript"})

	deadline := time.Now().Add(2 * time.Second)
	var got api.BuildImageResponse
	for time.Now().Before(deadline) {
		got, _ = m.Get(resp.ID)
		if got.Status == api.ImageBuildStatusFailed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.Status != api.ImageBuildStatusFailed {
		t.Fatalf("status = %q", got.Status)
	}
	if got.ErrorMessage != "exit code 1" {
		t.Fatalf("error = %q", got.ErrorMessage)
	}
	if len(m.AllowedImagesFor("alice")) != 0 {
		t.Fatal("failed build should not produce an allowlisted image")
	}
}

func TestSubmitDockerCallError(t *testing.T) {
	fd := &fakeDocker{buildErr: errors.New("daemon offline")}
	m := newTestManager(t, fd, nil)
	resp, _ := m.Submit(context.Background(), "alice",
		api.BuildImageRequest{Template: "node22-typescript"})
	deadline := time.Now().Add(2 * time.Second)
	var got api.BuildImageResponse
	for time.Now().Before(deadline) {
		got, _ = m.Get(resp.ID)
		if got.Status != api.ImageBuildStatusQueued && got.Status != api.ImageBuildStatusBuilding {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.Status != api.ImageBuildStatusFailed {
		t.Fatalf("status = %q", got.Status)
	}
}

func TestGetUnknown(t *testing.T) {
	m := newTestManager(t, &fakeDocker{}, nil)
	if _, err := m.Get("unknown-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if _, err := m.Log("unknown-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Log expected ErrNotFound, got %v", err)
	}
}

func TestDiskRegistryRejectsNonAllowlist(t *testing.T) {
	d := NewDiskRegistry(t.TempDir())
	if _, err := d.Resolve("evil"); err == nil {
		t.Fatal("disk registry should reject non-allowlist templates")
	}
}

func TestDiskRegistryReadsCommittedTemplate(t *testing.T) {
	tmp := t.TempDir()
	dir := tmp + "/node22-typescript"
	if err := writeFile(dir+"/Dockerfile", "FROM node:22\n"); err != nil {
		t.Fatal(err)
	}
	d := NewDiskRegistry(tmp)
	body, err := d.Resolve("node22-typescript")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.Contains(body, "FROM node:22") {
		t.Fatalf("body = %q", body)
	}
}

func TestNewRequiresDockerAndRegistry(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error when Docker is nil")
	}
	if _, err := New(Config{Docker: &fakeDocker{}}); err == nil {
		t.Fatal("expected error when Templates is nil")
	}
}
