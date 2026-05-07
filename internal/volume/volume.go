// Package volume manages the lifecycle of named Docker volumes that
// outlive the session that created them. Volumes are tracked in an
// in-memory table (consistent with how the session manager handles its
// own state) and the docker volume name itself encodes the durable
// identifier: `devenv-{session_id}-{volume_name}`.
//
// Retention model:
//
//   - When a session is reaped, its volumes do NOT cascade-delete. They
//     stay in the volume table marked `released` until either a follow-up
//     session reattaches them (via POST /v1/sessions/{id}/attach-volumes)
//     or the GC sweeps them past `RetentionUntil`.
//   - The `gc` admin endpoint (POST /v1/admin/volumes/gc) walks the
//     table, calls Docker's VolumeRemove for any volume past retention,
//     and updates the row's status. Errors do not abort the sweep.
package volume

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/SAY-5/devenv-manager/internal/docker"
	"github.com/SAY-5/devenv-manager/pkg/api"
	"github.com/google/uuid"
)

// DefaultRetention is how long a volume survives after the session that
// created it is reaped. Conservative default; operators can override.
const DefaultRetention = 24 * time.Hour

// ErrNotFound is returned when a volume ID is not in the table.
var ErrNotFound = errors.New("volume: not found")

// Manager tracks named volumes and translates the manager-level model
// into Docker volume operations.
type Manager struct {
	mu        sync.RWMutex
	volumes   map[string]*api.Volume
	docker    docker.Client
	now       func() time.Time
	retention time.Duration
}

// Config configures a Manager.
type Config struct {
	Docker    docker.Client
	Now       func() time.Time
	Retention time.Duration
}

// New constructs a Manager. The now function and retention are
// injectable so tests can drive the GC clock.
func New(cfg Config) *Manager {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Retention == 0 {
		cfg.Retention = DefaultRetention
	}
	return &Manager{
		volumes:   make(map[string]*api.Volume),
		docker:    cfg.Docker,
		now:       cfg.Now,
		retention: cfg.Retention,
	}
}

// CreateForSession creates a Docker named volume for the given session
// and records it in the table. The Docker volume name is
// `devenv-{session_id}-{name}` so the daemon side carries the durable
// identity even if the manager process is restarted with no state.
func (m *Manager) CreateForSession(ctx context.Context, sessionID string, spec api.VolumeSpec) (api.Volume, error) {
	if spec.Name == "" {
		return api.Volume{}, errors.New("volume: name is required")
	}
	if spec.MountPath == "" {
		return api.Volume{}, errors.New("volume: mount_path is required")
	}
	id := uuid.NewString()
	dockerName := fmt.Sprintf("devenv-%s-%s", shortID(sessionID), spec.Name)
	now := m.now()
	labels := map[string]string{
		docker.LabelID:       sessionID,
		"com.devenv.volume":  spec.Name,
		"com.devenv.created": now.UTC().Format(time.RFC3339),
	}
	dockerID, err := m.docker.CreateVolume(ctx, dockerName, labels)
	if err != nil {
		return api.Volume{}, err
	}
	v := api.Volume{
		ID:             id,
		SessionID:      sessionID,
		Name:           spec.Name,
		DockerVolumeID: dockerID,
		MountPath:      spec.MountPath,
		RetentionUntil: now.Add(m.retention),
		Status:         api.VolumeStatusActive,
		CreatedAt:      now,
	}
	m.mu.Lock()
	m.volumes[id] = &v
	m.mu.Unlock()
	return v, nil
}

// Get returns the volume by ID.
func (m *Manager) Get(id string) (api.Volume, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.volumes[id]
	if !ok {
		return api.Volume{}, ErrNotFound
	}
	return *v, nil
}

// List returns volumes filtered by session id; an empty session id
// returns all volumes. Output is sorted by CreatedAt ascending.
func (m *Manager) List(sessionID string) []api.Volume {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]api.Volume, 0)
	for _, v := range m.volumes {
		if sessionID != "" && v.SessionID != sessionID {
			continue
		}
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// MarkReleased flags a session's volumes as released (the session is
// being reaped). Retention timers are reset relative to "now" so the
// declared retention applies from the release moment, not from
// creation.
func (m *Manager) MarkReleased(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for _, v := range m.volumes {
		if v.SessionID == sessionID && v.Status == api.VolumeStatusActive {
			v.Status = api.VolumeStatusReleased
			v.RetentionUntil = now.Add(m.retention)
		}
	}
}

// Delete removes a single volume from Docker and the table.
func (m *Manager) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	v, ok := m.volumes[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	dockerName := v.DockerVolumeID
	delete(m.volumes, id)
	m.mu.Unlock()
	return m.docker.RemoveVolume(ctx, dockerName, false)
}

// GC walks the table and removes volumes past their retention. Errors
// from individual removals are accumulated and returned alongside the
// list of successfully removed volume IDs; the GC never aborts on the
// first failure because partial progress is the desired behaviour.
func (m *Manager) GC(ctx context.Context) ([]string, []error) {
	now := m.now()
	m.mu.Lock()
	candidates := make([]*api.Volume, 0)
	for _, v := range m.volumes {
		if v.Status == api.VolumeStatusGCed {
			continue
		}
		if !v.RetentionUntil.IsZero() && v.RetentionUntil.Before(now) {
			candidates = append(candidates, v)
		}
	}
	m.mu.Unlock()

	removed := make([]string, 0, len(candidates))
	errs := make([]error, 0)
	for _, v := range candidates {
		if err := m.docker.RemoveVolume(ctx, v.DockerVolumeID, false); err != nil {
			errs = append(errs, fmt.Errorf("gc %s: %w", v.ID, err))
			continue
		}
		m.mu.Lock()
		if cur, ok := m.volumes[v.ID]; ok {
			cur.Status = api.VolumeStatusGCed
		}
		m.mu.Unlock()
		removed = append(removed, v.ID)
	}
	return removed, errs
}

// shortID truncates a UUID-shaped string to its first 8 hex characters
// so docker volume names stay short and human-grepable.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
