package reaper

import (
	"testing"
	"time"

	"github.com/SAY-5/devenv-manager/internal/docker"
)

// FuzzEvaluateDecisionTable feeds random combinations of lease times,
// creator PIDs, and PID-alive answers into the reaper's decision matrix.
// The invariants under test mirror the documented matrix:
//
//  1. Empty SessionID -> Keep (no label, not ours).
//  2. Lease in the past -> ReapTTL, regardless of the orphan signal.
//  3. CreatedByPID == ourPID -> never Orphan.
//  4. CreatedByPID > 0, different from ours, dead -> ReapOrphan.
//  5. Otherwise Keep.
func FuzzEvaluateDecisionTable(f *testing.F) {
	f.Add(int64(0), int64(0), 100, 100, true)
	f.Add(int64(-1), int64(0), 100, 999, false)
	f.Add(int64(60), int64(0), 100, 999, false)
	f.Add(int64(60), int64(0), 100, 100, true)
	f.Add(int64(60), int64(0), 100, 0, true)

	f.Fuzz(func(t *testing.T, leaseDeltaSec int64, _ int64, ourPID int, theirPID int, alive bool) {
		// Clamp the lease delta to a reasonable band; outside it the time
		// arithmetic loses precision but the decision boundary is unchanged.
		if leaseDeltaSec < -1<<30 {
			leaseDeltaSec = -1 << 30
		}
		if leaseDeltaSec > 1<<30 {
			leaseDeltaSec = 1 << 30
		}
		if ourPID < 1 {
			ourPID = 1
		}
		if theirPID < 0 {
			theirPID = 0
		}

		now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
		info := docker.ContainerInfo{
			ID:           "c",
			SessionID:    "s",
			LeaseUntil:   now.Add(time.Duration(leaseDeltaSec) * time.Second),
			CreatedByPID: theirPID,
		}
		r := New(&fakeClient{}, Config{
			Now:        func() time.Time { return now },
			CurrentPID: ourPID,
			PIDAlive:   func(int) bool { return alive },
		})
		got := r.Evaluate(info)

		switch {
		case info.LeaseUntil.Before(now):
			if got != DecisionReapTTL {
				t.Fatalf("expired lease should ReapTTL, got %v (lease=%v now=%v)", got, info.LeaseUntil, now)
			}
		case theirPID > 0 && theirPID != ourPID && !alive:
			if got != DecisionReapOrphan {
				t.Fatalf("dead foreign pid should ReapOrphan, got %v", got)
			}
		default:
			if got != DecisionKeep {
				t.Fatalf("expected Keep, got %v (lease delta=%ds ourPID=%d theirPID=%d alive=%v)",
					got, leaseDeltaSec, ourPID, theirPID, alive)
			}
		}

		// Empty SessionID is always Keep. Verify with the same params.
		blank := info
		blank.SessionID = ""
		if r.Evaluate(blank) != DecisionKeep {
			t.Fatalf("empty SessionID must Keep")
		}
	})
}

// FuzzDecisionStringStable confirms String() is total — every Decision
// integer round-trips to a non-empty label, including unknown sentinels
// which must collapse to "keep".
func FuzzDecisionStringStable(f *testing.F) {
	for i := -3; i <= 5; i++ {
		f.Add(int8(i))
	}
	f.Fuzz(func(t *testing.T, raw int8) {
		d := Decision(raw)
		s := d.String()
		if s == "" {
			t.Fatalf("empty label for %d", raw)
		}
		switch d {
		case DecisionReapTTL:
			if s != "ttl" {
				t.Fatalf("ReapTTL label drift: %q", s)
			}
		case DecisionReapOrphan:
			if s != "orphan" {
				t.Fatalf("ReapOrphan label drift: %q", s)
			}
		default:
			if s != "keep" {
				t.Fatalf("unknown decision %d should label as keep, got %q", raw, s)
			}
		}
	})
}
