package session

import (
	"testing"
	"time"

	"github.com/SAY-5/devenv-manager/internal/docker"
)

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestAddAndGet(t *testing.T) {
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	m := New(fixedClock(now))
	m.Add(&Session{ID: "s1", ContainerID: "c1", CreatedAt: now, ExpiresAt: now.Add(time.Minute)})
	got, err := m.Get("s1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ContainerID != "c1" {
		t.Fatalf("ContainerID mismatch: %q", got.ContainerID)
	}
}

func TestGetMissing(t *testing.T) {
	m := New(nil)
	if _, err := m.Get("nope"); err == nil {
		t.Fatal("expected ErrNotFound")
	}
}

func TestRefreshExtendsTTL(t *testing.T) {
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	m := New(fixedClock(now))
	m.Add(&Session{ID: "s1", CreatedAt: now, ExpiresAt: now.Add(10 * time.Second)})

	updated, err := m.Refresh("s1", 60*time.Second)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	want := now.Add(60 * time.Second)
	if !updated.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", updated.ExpiresAt, want)
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	m := New(nil)
	m.Add(&Session{ID: "s1"})
	m.Remove("s1")
	m.Remove("s1") // no panic
	if _, err := m.Get("s1"); err == nil {
		t.Fatal("expected gone after Remove")
	}
}

func TestListSortedByCreatedAt(t *testing.T) {
	m := New(nil)
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	m.Add(&Session{ID: "b", CreatedAt: t0.Add(time.Second)})
	m.Add(&Session{ID: "a", CreatedAt: t0})
	m.Add(&Session{ID: "c", CreatedAt: t0.Add(2 * time.Second)})

	got := m.List()
	if len(got) != 3 {
		t.Fatalf("len=%d", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "b" || got[2].ID != "c" {
		t.Fatalf("order: %v %v %v", got[0].ID, got[1].ID, got[2].ID)
	}
}

func TestHydrateFromContainers(t *testing.T) {
	m := New(nil)
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	m.HydrateFromContainers([]docker.ContainerInfo{
		{ID: "c1", Image: "alpine:3.20", State: "running", Created: t0,
			SessionID: "s1", Owner: "anonymous", LeaseUntil: t0.Add(time.Minute)},
		{ID: "c2"}, // no SessionID, skipped
	})
	got, err := m.Get("s1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ContainerID != "c1" || got.Image != "alpine:3.20" {
		t.Fatalf("hydrated = %+v", got)
	}
	if _, err := m.Get(""); err == nil {
		t.Fatal("expected empty SessionID to be skipped")
	}
}
