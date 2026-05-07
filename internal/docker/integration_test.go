//go:build integration

package docker

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func skipIfNoDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("RUN_INTEGRATION") == "" {
		t.Skip("set RUN_INTEGRATION=1 to enable")
	}
}

func TestIntegrationCreateStartExecRemove(t *testing.T) {
	skipIfNoDocker(t)
	c, err := New()
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if _, err := c.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	now := time.Now()
	spec := ContainerSpec{
		ID:         "integ-test-" + now.Format("150405"),
		Image:      "alpine:3.20",
		Owner:      "integration",
		LeaseUntil: now.Add(2 * time.Minute),
		CreatedAt:  now,
		CreatedBy:  os.Getpid(),
		Command:    []string{"/bin/sh"},
	}

	cid, err := c.CreateContainer(ctx, spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_ = c.RemoveContainer(context.Background(), cid, true)
	})

	if err := c.StartContainer(ctx, cid); err != nil {
		t.Fatalf("start: %v", err)
	}

	conn, err := c.ExecAttach(ctx, cid, []string{"/bin/sh", "-c", "echo hello-integration"}, false)
	if err != nil {
		t.Fatalf("exec attach: %v", err)
	}
	defer conn.Close()

	// Drain a couple seconds.
	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4096)
		var collected []byte
		for {
			n, rerr := conn.Read(buf)
			if n > 0 {
				collected = append(collected, buf[:n]...)
			}
			if rerr != nil {
				break
			}
			if rerr == io.EOF {
				break
			}
			if len(collected) > 0 {
				break
			}
		}
		done <- collected
	}()

	select {
	case got := <-done:
		if !strings.Contains(string(got), "hello-integration") {
			t.Fatalf("unexpected output: %q", got)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("timeout waiting for output")
	}

	// Verify the container is listed by label.
	infos, err := c.ListLabeledContainers(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, info := range infos {
		if info.ID == cid && info.SessionID == spec.ID {
			found = true
			if info.CreatedByPID != os.Getpid() {
				t.Fatalf("PID label not preserved: %d vs %d", info.CreatedByPID, os.Getpid())
			}
		}
	}
	if !found {
		t.Fatal("container not found in labeled listing")
	}
}
