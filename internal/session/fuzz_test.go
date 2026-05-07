package session

import (
	"testing"
	"time"
)

// FuzzRefreshTTL drives random TTL durations through the manager's Refresh
// path. The contract is: ExpiresAt must equal now()+ttl after the call,
// regardless of how silly the inputs are. Refresh on an unknown ID must
// return ErrNotFound and never advance any clock.
func FuzzRefreshTTL(f *testing.F) {
	f.Add(int64(0), int64(60))
	f.Add(int64(-3600), int64(1))
	f.Add(int64(1<<31), int64(1<<32))
	f.Add(int64(0), int64(-1))
	f.Add(int64(1700000000), int64(1<<40))

	f.Fuzz(func(t *testing.T, nowUnix int64, ttlNanos int64) {
		// Clamp to a sane band so time.Time arithmetic doesn't overflow into
		// undefined behaviour. The bounds are wide enough that the math still
		// covers the realistic operating range and beyond.
		if nowUnix < -1<<32 {
			nowUnix = -1 << 32
		}
		if nowUnix > 1<<32 {
			nowUnix = 1 << 32
		}
		if ttlNanos < -int64(time.Hour) {
			ttlNanos = -int64(time.Hour)
		}
		if ttlNanos > int64(365*24*time.Hour) {
			ttlNanos = int64(365 * 24 * time.Hour)
		}
		now := time.Unix(nowUnix, 0).UTC()
		ttl := time.Duration(ttlNanos)

		m := New(func() time.Time { return now })
		m.Add(&Session{ID: "s1", CreatedAt: now})
		got, err := m.Refresh("s1", ttl)
		if err != nil {
			t.Fatalf("Refresh of present id failed: %v", err)
		}
		want := now.Add(ttl)
		if !got.ExpiresAt.Equal(want) {
			t.Fatalf("ExpiresAt = %v, want %v (now=%v ttl=%v)", got.ExpiresAt, want, now, ttl)
		}
		// Unknown ID must always return ErrNotFound, even when the manager has
		// other sessions in its table.
		if _, err := m.Refresh("missing", ttl); err == nil {
			t.Fatalf("Refresh of missing id should fail; got nil")
		}
	})
}

// FuzzExpirySequence stresses the invariant that Refresh is monotonic for a
// growing-clock + non-decreasing-ttl sequence. Going backwards in time is
// allowed (the clock is injectable in tests); the assertion only fires when
// we feed a non-decreasing pair.
func FuzzExpirySequence(f *testing.F) {
	f.Add(int64(0), int64(1), int64(60))
	f.Add(int64(0), int64(0), int64(1))
	f.Add(int64(100), int64(50), int64(10)) // backwards clock; assertion skipped

	f.Fuzz(func(t *testing.T, t0 int64, t1 int64, ttlSec int64) {
		if ttlSec < 0 || ttlSec > int64(365*24*3600) {
			return
		}
		var now time.Time
		clock := func() time.Time { return now }
		now = time.Unix(t0, 0).UTC()
		m := New(clock)
		m.Add(&Session{ID: "s1", CreatedAt: now})
		first, err := m.Refresh("s1", time.Duration(ttlSec)*time.Second)
		if err != nil {
			t.Fatalf("first Refresh: %v", err)
		}
		now = time.Unix(t1, 0).UTC()
		second, err := m.Refresh("s1", time.Duration(ttlSec)*time.Second)
		if err != nil {
			t.Fatalf("second Refresh: %v", err)
		}
		if t1 >= t0 {
			if second.ExpiresAt.Before(first.ExpiresAt) {
				t.Fatalf("monotonic violation: t0=%d -> %v, t1=%d -> %v", t0, first.ExpiresAt, t1, second.ExpiresAt)
			}
		}
	})
}
