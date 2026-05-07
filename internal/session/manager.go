// Package session manages in-memory state for active container sessions.
// The authoritative identity layer is the set of Docker labels on the
// container itself; this struct is a hot cache that the reaper rebuilds
// from labels on startup.
package session

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/SAY-5/devenv-manager/internal/docker"
)

// Session is the in-memory record for an active session.
type Session struct {
	ID          string
	ContainerID string
	Image       string
	Owner       string
	WorkingDir  string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// Manager tracks active sessions with TTL.
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	now      func() time.Time
}

// New returns a Manager. The now function lets tests inject a clock.
func New(now func() time.Time) *Manager {
	if now == nil {
		now = time.Now
	}
	return &Manager{
		sessions: make(map[string]*Session),
		now:      now,
	}
}

// ErrNotFound is returned when a session ID is not in the table.
var ErrNotFound = errors.New("session: not found")

// Add inserts a new session. Returns the inserted record.
func (m *Manager) Add(s *Session) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.ID] = s
	return s
}

// Get returns the session by ID.
func (m *Manager) Get(id string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *s
	return &cp, nil
}

// Remove deletes a session by ID. No-op if absent.
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}

// Refresh extends the session TTL by ttl from now. Returns the updated
// session or ErrNotFound.
func (m *Manager) Refresh(id string, ttl time.Duration) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	s.ExpiresAt = m.now().Add(ttl)
	cp := *s
	return &cp, nil
}

// List returns sessions sorted by CreatedAt ascending.
func (m *Manager) List() []Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// HydrateFromContainers rebuilds the session table from Docker labels.
// This is what makes the in-memory table durable: after a service restart,
// the labels written into the daemon are read back into the cache so
// existing sessions remain addressable through the API.
func (m *Manager) HydrateFromContainers(infos []docker.ContainerInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, info := range infos {
		if info.SessionID == "" {
			continue
		}
		m.sessions[info.SessionID] = &Session{
			ID:          info.SessionID,
			ContainerID: info.ID,
			Image:       info.Image,
			Owner:       info.Owner,
			CreatedAt:   info.Created,
			ExpiresAt:   info.LeaseUntil,
		}
	}
}
