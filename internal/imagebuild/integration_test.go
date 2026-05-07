//go:build integration

package imagebuild

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SAY-5/devenv-manager/internal/docker"
	"github.com/SAY-5/devenv-manager/pkg/api"
)

func skipIfNoDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("RUN_INTEGRATION") == "" {
		t.Skip("set RUN_INTEGRATION=1 to enable")
	}
}

// TestIntegrationBuildAndRun drives the full pipeline against a real
// daemon: build the node22-typescript template, wait until it's
// ready, start a container from the built image, and confirm
// `node --version` reports v22. This is the test the v4 spec asks for.
func TestIntegrationBuildAndRun(t *testing.T) {
	skipIfNoDocker(t)
	dc, err := docker.New()
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = dc.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := dc.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	// Use a stripped-down template body that's quick to build but still
	// produces a node:22-alpine image so `node --version` is meaningful.
	bodies := map[string]string{
		"node22-typescript": "FROM node:22-alpine\n" +
			"RUN node --version > /etc/devenv-node-version\n",
	}
	mgr, err := New(Config{
		Docker:       dc,
		Templates:    NewMemoryRegistry(bodies),
		LogsDir:      t.TempDir(),
		MaxWallClock: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// 1. Build.
	resp, err := mgr.Submit(ctx, "integration",
		api.BuildImageRequest{Template: "node22-typescript"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	deadline := time.Now().Add(4 * time.Minute)
	var built api.BuildImageResponse
	for time.Now().Before(deadline) {
		built, _ = mgr.Get(resp.ID)
		if built.Status == api.ImageBuildStatusReady ||
			built.Status == api.ImageBuildStatusFailed {
			break
		}
		time.Sleep(time.Second)
	}
	if built.Status != api.ImageBuildStatusReady {
		logs, _ := mgr.Log(resp.ID)
		t.Fatalf("build did not become ready: %+v\nlogs:\n%s", built, logs)
	}

	// 2. Spawn a container from the built image and run `node --version`.
	spec := docker.ContainerSpec{
		ID:         "imagebuild-integration-" + time.Now().Format("150405"),
		Image:      built.ImageRef,
		Owner:      "integration",
		LeaseUntil: time.Now().Add(2 * time.Minute),
		CreatedAt:  time.Now(),
		CreatedBy:  os.Getpid(),
		Command:    []string{"/bin/sh"},
	}
	cid, err := dc.CreateContainer(ctx, spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_ = dc.RemoveContainer(context.Background(), cid, true)
	})
	if err := dc.StartContainer(ctx, cid); err != nil {
		t.Fatalf("start: %v", err)
	}

	// 3. exec `node --version` and confirm v22.
	conn, err := dc.ExecAttach(ctx, cid, []string{"node", "--version"}, false)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	defer func() { _ = conn.Close() }()
	out := make([]byte, 0, 64)
	buf := make([]byte, 1024)
	deadline2 := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline2) {
		n, rerr := conn.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if rerr != nil || strings.Contains(string(out), "v22") {
			break
		}
	}
	if !strings.Contains(string(out), "v22") {
		t.Fatalf("node --version output = %q, want v22*", out)
	}
}
