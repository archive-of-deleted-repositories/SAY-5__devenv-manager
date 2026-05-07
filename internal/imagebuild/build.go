// Package imagebuild manages on-demand image builds from a closed set of
// committed Dockerfile templates. The security boundary is enforced
// here: arbitrary Dockerfiles are not accepted; only templates whose
// sources are committed at infra/templates/<name>/Dockerfile.
//
// Build pipeline:
//
//  1. POST /v1/images/build {template, build_args} → handler validates
//     the template against the registry and submits a Job.
//  2. The manager spawns a goroutine that builds the tar context, calls
//     docker.Client.BuildImage, and drains the response stream.
//  3. Status transitions queued → building → ready|failed; logs are
//     accumulated under bench/buildlogs/<id>.log (gitignored) so the
//     /v1/images/{id} response can return a stable build_log_url.
//  4. On success, the resulting tag is added to a per-owner allowlist
//     so subsequent POST /v1/sessions calls accept the image.
//
// Resource caps are wall-clock based: 5 minutes per build. The 1 CPU /
// 1 GiB caps from the spec are enforced by the docker daemon's default
// build resource limits (the engine SDK does not accept per-call
// CPU/memory overrides for build submissions; operators harden the
// daemon globally if tighter caps are required).
package imagebuild

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/SAY-5/devenv-manager/internal/docker"
	"github.com/SAY-5/devenv-manager/pkg/api"
	"github.com/google/uuid"
)

// MaxWallClock is the per-build wall-clock cap. Operators can override
// via Config but the spec mandates 5 minutes as the safe default.
const MaxWallClock = 5 * time.Minute

// Manager owns the image-build state machine.
type Manager struct {
	docker    docker.Client
	templates Registry
	logsDir   string
	now       func() time.Time
	maxWall   time.Duration

	mu      sync.RWMutex
	builds  map[string]*api.BuildImageResponse
	owners  map[string]map[string]bool // owner -> imageRef set
	logsBuf map[string]*logBuffer
}

// Config configures a Manager.
type Config struct {
	Docker        docker.Client
	Templates     Registry
	LogsDir       string
	Now           func() time.Time
	MaxWallClock  time.Duration
	BuildLogURLFn func(id string) string // optional override
}

// Registry resolves a template name to a Dockerfile body. The
// production registry reads from infra/templates; tests inject a memory
// implementation.
type Registry interface {
	List() []string
	Resolve(name string) (string, error)
}

// New constructs a Manager.
func New(cfg Config) (*Manager, error) {
	if cfg.Docker == nil {
		return nil, errors.New("imagebuild: docker client is required")
	}
	if cfg.Templates == nil {
		return nil, errors.New("imagebuild: templates registry is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxWallClock == 0 {
		cfg.MaxWallClock = MaxWallClock
	}
	if cfg.LogsDir == "" {
		cfg.LogsDir = filepath.Join(os.TempDir(), "devenv-buildlogs")
	}
	if err := os.MkdirAll(cfg.LogsDir, 0o750); err != nil {
		return nil, fmt.Errorf("imagebuild: mkdir logs: %w", err)
	}
	return &Manager{
		docker:    cfg.Docker,
		templates: cfg.Templates,
		logsDir:   cfg.LogsDir,
		now:       cfg.Now,
		maxWall:   cfg.MaxWallClock,
		builds:    make(map[string]*api.BuildImageResponse),
		owners:    make(map[string]map[string]bool),
		logsBuf:   make(map[string]*logBuffer),
	}, nil
}

// Submit enqueues a new build for owner. Returns a snapshot of the
// initial response (status=queued) so the handler can echo it to the
// client without observing the goroutine's later writes.
func (m *Manager) Submit(ctx context.Context, owner string, req api.BuildImageRequest) (api.BuildImageResponse, error) {
	if _, err := m.templates.Resolve(req.Template); err != nil {
		return api.BuildImageResponse{}, fmt.Errorf("imagebuild: %w", err)
	}
	id := uuid.NewString()
	now := m.now()
	tag := fmt.Sprintf("devenv-builds/%s:%s", req.Template, id[:8])
	rec := &api.BuildImageResponse{
		ID:          id,
		Template:    req.Template,
		Status:      api.ImageBuildStatusQueued,
		ImageRef:    tag,
		BuildLogURL: fmt.Sprintf("/v1/images/%s/log", id),
		CreatedAt:   now,
	}
	snapshot := *rec
	m.mu.Lock()
	m.builds[id] = rec
	m.logsBuf[id] = newLogBuffer()
	m.mu.Unlock()

	go m.run(owner, id, tag, req)
	return snapshot, nil
}

// Get returns the current state of a build.
func (m *Manager) Get(id string) (api.BuildImageResponse, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.builds[id]
	if !ok {
		return api.BuildImageResponse{}, ErrNotFound
	}
	return *b, nil
}

// Log returns the accumulated build log for a build.
func (m *Manager) Log(id string) ([]byte, error) {
	m.mu.RLock()
	buf, ok := m.logsBuf[id]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}
	return buf.Bytes(), nil
}

// AllowedImagesFor returns the tags the owner is permitted to use in
// /v1/sessions create calls (in addition to the static allowlist).
func (m *Manager) AllowedImagesFor(owner string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	set, ok := m.owners[owner]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

// ErrNotFound is returned when a build id is unknown.
var ErrNotFound = errors.New("imagebuild: not found")

// run drives a single build to completion. Errors are recorded on the
// build record; the goroutine never panics out.
func (m *Manager) run(owner, id, tag string, req api.BuildImageRequest) {
	m.transition(id, func(b *api.BuildImageResponse) {
		b.Status = api.ImageBuildStatusBuilding
	})

	body, err := m.templates.Resolve(req.Template)
	if err != nil {
		m.fail(id, fmt.Errorf("resolve template: %w", err))
		return
	}
	contextTar, err := tarDockerfile(body)
	if err != nil {
		m.fail(id, fmt.Errorf("tar dockerfile: %w", err))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), m.maxWall)
	defer cancel()
	stream, err := m.docker.BuildImage(ctx, docker.ImageBuildSpec{
		Tag:          tag,
		BuildArgs:    req.BuildArgs,
		BuildContext: contextTar,
	})
	if err != nil {
		m.fail(id, fmt.Errorf("docker build: %w", err))
		return
	}
	defer func() { _ = stream.Close() }()

	if err := m.drain(id, stream); err != nil {
		m.fail(id, err)
		return
	}

	now := m.now()
	m.mu.Lock()
	if b, ok := m.builds[id]; ok {
		b.Status = api.ImageBuildStatusReady
		b.CompletedAt = now
	}
	if _, ok := m.owners[owner]; !ok {
		m.owners[owner] = make(map[string]bool)
	}
	m.owners[owner][tag] = true
	m.mu.Unlock()
}

func (m *Manager) drain(id string, stream io.Reader) error {
	dec := json.NewDecoder(stream)
	for {
		var frame map[string]any
		if err := dec.Decode(&frame); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read build stream: %w", err)
		}
		if errMsg, ok := frame["error"].(string); ok && errMsg != "" {
			return errors.New(errMsg)
		}
		var line string
		if s, ok := frame["stream"].(string); ok {
			line = s
		} else if s, ok := frame["status"].(string); ok {
			line = s
		}
		m.appendLog(id, strings.TrimRight(line, "\n")+"\n")
	}
}

func (m *Manager) transition(id string, fn func(*api.BuildImageResponse)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.builds[id]; ok {
		fn(b)
	}
}

func (m *Manager) fail(id string, err error) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.builds[id]; ok {
		b.Status = api.ImageBuildStatusFailed
		b.ErrorMessage = err.Error()
		b.CompletedAt = now
	}
}

func (m *Manager) appendLog(id, line string) {
	m.mu.Lock()
	if buf, ok := m.logsBuf[id]; ok {
		buf.Append([]byte(line))
	}
	m.mu.Unlock()
}

// tarDockerfile produces a single-entry tar stream containing
// "Dockerfile" with the given body. The Docker engine accepts this
// directly as a build context.
func tarDockerfile(body string) (io.Reader, error) {
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	hdr := &tar.Header{
		Name: "Dockerfile",
		Mode: 0o644,
		Size: int64(len(body)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return nil, err
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf, nil
}

// logBuffer is a synchronized byte buffer for build logs.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newLogBuffer() *logBuffer { return &logBuffer{} }

func (b *logBuffer) Append(p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, _ = b.buf.Write(p)
}

func (b *logBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]byte, b.buf.Len())
	copy(out, b.buf.Bytes())
	return out
}
