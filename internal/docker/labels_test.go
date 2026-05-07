package docker

import (
	"slices"
	"testing"
)

func TestAllLabelsContainsExpectedKeys(t *testing.T) {
	all := AllLabels()
	for _, want := range []string{LabelID, LabelOwner, LabelCreatedAt, LabelLeaseUntil, LabelCreatedByPID} {
		if !slices.Contains(all, want) {
			t.Fatalf("missing key %q in AllLabels(): %v", want, all)
		}
	}
}

func TestLabelKeysAreNamespaced(t *testing.T) {
	for _, k := range AllLabels() {
		if len(k) < len("com.devenv.") || k[:len("com.devenv.")] != "com.devenv." {
			t.Fatalf("label %q is not under com.devenv. namespace", k)
		}
	}
}

func TestParseLabels(t *testing.T) {
	info := ContainerInfo{
		Labels: map[string]string{
			LabelID:           "abc",
			LabelOwner:        "tester",
			LabelLeaseUntil:   "1700000000",
			LabelCreatedByPID: "42",
		},
	}
	parseLabels(&info)
	if info.SessionID != "abc" {
		t.Fatalf("SessionID = %q", info.SessionID)
	}
	if info.Owner != "tester" {
		t.Fatalf("Owner = %q", info.Owner)
	}
	if info.LeaseUntil.Unix() != 1700000000 {
		t.Fatalf("LeaseUntil = %v", info.LeaseUntil)
	}
	if info.CreatedByPID != 42 {
		t.Fatalf("CreatedByPID = %d", info.CreatedByPID)
	}
}

func TestParseLabelsHandlesGarbage(t *testing.T) {
	info := ContainerInfo{
		Labels: map[string]string{
			LabelID:           "abc",
			LabelLeaseUntil:   "not-a-number",
			LabelCreatedByPID: "also-bad",
		},
	}
	parseLabels(&info) // should not panic
	if info.SessionID != "abc" {
		t.Fatalf("SessionID = %q", info.SessionID)
	}
	if !info.LeaseUntil.IsZero() {
		t.Fatalf("LeaseUntil should be zero on parse failure, got %v", info.LeaseUntil)
	}
	if info.CreatedByPID != 0 {
		t.Fatalf("CreatedByPID should be 0 on parse failure, got %d", info.CreatedByPID)
	}
}
