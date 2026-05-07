package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SAY-5/devenv-manager/internal/docker"
	"github.com/SAY-5/devenv-manager/internal/imagebuild"
	"github.com/SAY-5/devenv-manager/internal/session"
	apiTypes "github.com/SAY-5/devenv-manager/pkg/api"
)

// buildAwareClient is a fakeClient that tracks BuildImage calls and
// returns a configurable build stream — this is what makes the
// build-and-run integration assertable end-to-end inside the api test
// without spinning up a real daemon.
type buildAwareClient struct {
	*fakeClient
	mu          sync.Mutex
	buildStream string
	builtTags   []string
}

func (b *buildAwareClient) BuildImage(ctx context.Context, spec docker.ImageBuildSpec) (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.builtTags = append(b.builtTags, spec.Tag)
	return io.NopCloser(strings.NewReader(b.buildStream)), nil
}

func newImageBuildServer(t *testing.T, buildStream string) (*httptest.Server, *imagebuild.Manager, *buildAwareClient) {
	t.Helper()
	fc := &buildAwareClient{fakeClient: &fakeClient{}, buildStream: buildStream}
	reg := imagebuild.NewMemoryRegistry(map[string]string{
		"node22-typescript": "FROM node:22-alpine\nRUN node --version > /etc/devenv-node-version\n",
		"python312-poetry":  "FROM python:3.12-slim\n",
		"rust-stable":       "FROM rust:1-slim\n",
	})
	mgr, err := imagebuild.New(imagebuild.Config{
		Docker:       fc,
		Templates:    reg,
		LogsDir:      t.TempDir(),
		MaxWallClock: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("imagebuild.New: %v", err)
	}
	srv := NewServer(Config{
		Docker:      fc,
		Sessions:    session.New(time.Now),
		ImageBuilds: mgr,
		DefaultTTL:  60 * time.Second,
		MaxTTL:      5 * time.Minute,
		Owner:       "tester",
		PID:         12345,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, mgr, fc
}

func TestBuildEndpointAcceptsAllowlistedTemplate(t *testing.T) {
	stream := `{"stream":"FROM node:22-alpine\n"}` + "\n" +
		`{"stream":"Successfully built abc\n"}` + "\n"
	ts, _, _ := newImageBuildServer(t, stream)

	body, _ := json.Marshal(apiTypes.BuildImageRequest{Template: "node22-typescript"})
	resp, err := http.Post(ts.URL+"/v1/images/build", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out apiTypes.BuildImageResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.ID == "" || out.Status != apiTypes.ImageBuildStatusQueued {
		t.Fatalf("response = %+v", out)
	}
	if out.BuildLogURL == "" {
		t.Fatal("build_log_url empty")
	}
}

func TestBuildEndpointRejectsUnknownTemplate(t *testing.T) {
	ts, _, _ := newImageBuildServer(t, "")
	body, _ := json.Marshal(apiTypes.BuildImageRequest{Template: "evil-template"})
	resp, err := http.Post(ts.URL+"/v1/images/build", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestBuildEndpointStatusPolling(t *testing.T) {
	stream := `{"stream":"FROM node:22-alpine\n"}` + "\n" +
		`{"stream":"Successfully built abc\n"}` + "\n"
	ts, _, _ := newImageBuildServer(t, stream)

	body, _ := json.Marshal(apiTypes.BuildImageRequest{Template: "node22-typescript"})
	resp, err := http.Post(ts.URL+"/v1/images/build", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var submitted apiTypes.BuildImageResponse
	_ = json.NewDecoder(resp.Body).Decode(&submitted)

	// Poll until ready.
	deadline := time.Now().Add(2 * time.Second)
	var got apiTypes.BuildImageResponse
	for time.Now().Before(deadline) {
		resp2, err := http.Get(ts.URL + "/v1/images/" + submitted.ID)
		if err != nil {
			t.Fatal(err)
		}
		_ = json.NewDecoder(resp2.Body).Decode(&got)
		resp2.Body.Close()
		if got.Status == apiTypes.ImageBuildStatusReady {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.Status != apiTypes.ImageBuildStatusReady {
		t.Fatalf("status never became ready: %+v", got)
	}

	// Build log endpoint returns the streamed content.
	resp3, err := http.Get(ts.URL + "/v1/images/" + submitted.ID + "/log")
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	logBody := new(bytes.Buffer)
	_, _ = logBody.ReadFrom(resp3.Body)
	if !strings.Contains(logBody.String(), "FROM node:22-alpine") {
		t.Fatalf("log missing build output: %q", logBody.String())
	}
}

func TestImagesIncludesBuiltAfterReady(t *testing.T) {
	stream := `{"stream":"Successfully built abc\n"}` + "\n"
	ts, mgr, _ := newImageBuildServer(t, stream)

	body, _ := json.Marshal(apiTypes.BuildImageRequest{Template: "node22-typescript"})
	resp, err := http.Post(ts.URL+"/v1/images/build", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var submitted apiTypes.BuildImageResponse
	_ = json.NewDecoder(resp.Body).Decode(&submitted)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := mgr.Get(submitted.ID)
		if got.Status == apiTypes.ImageBuildStatusReady {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	resp2, err := http.Get(ts.URL + "/v1/images")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var imgs apiTypes.ImagesResponse
	_ = json.NewDecoder(resp2.Body).Decode(&imgs)
	found := false
	for _, img := range imgs.Images {
		if strings.HasPrefix(img, "devenv-builds/node22-typescript:") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("built image not in /v1/images: %v", imgs.Images)
	}
}

// TestBuildAndRunCycle is the load-bearing integration test required by
// v4: build the node template through the build endpoint, poll until
// ready, then start a session with the built image and confirm the
// session-create call accepts the freshly-built tag (and that the tag
// is honored on the docker.CreateContainer side). The actual
// `node --version` assertion is exercised by the live-docker test suite
// (frontend e2e + manual run); here we prove the pipeline's contract:
// build → ready → allowed → session.
func TestBuildAndRunCycle(t *testing.T) {
	stream := `{"stream":"FROM node:22-alpine\n"}` + "\n" +
		`{"stream":"RUN node --version\n"}` + "\n" +
		`{"stream":"Successfully built abcdef\n"}` + "\n"
	ts, mgr, fc := newImageBuildServer(t, stream)

	// 1. Submit the build.
	body, _ := json.Marshal(apiTypes.BuildImageRequest{Template: "node22-typescript"})
	resp, err := http.Post(ts.URL+"/v1/images/build", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var submitted apiTypes.BuildImageResponse
	_ = json.NewDecoder(resp.Body).Decode(&submitted)
	if submitted.ID == "" {
		t.Fatal("missing build id")
	}

	// 2. Poll until ready.
	deadline := time.Now().Add(3 * time.Second)
	var built apiTypes.BuildImageResponse
	for time.Now().Before(deadline) {
		built, _ = mgr.Get(submitted.ID)
		if built.Status == apiTypes.ImageBuildStatusReady {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if built.Status != apiTypes.ImageBuildStatusReady {
		t.Fatalf("build never became ready: %+v", built)
	}
	tag := built.ImageRef
	if !strings.HasPrefix(tag, "devenv-builds/node22-typescript:") {
		t.Fatalf("unexpected image ref: %q", tag)
	}

	// 3. Start a session with the built image. The static allowlist
	// would reject this; the per-owner build allowlist is what lets it
	// through.
	createBody, _ := json.Marshal(apiTypes.CreateSessionRequest{Image: tag})
	resp2, err := http.Post(ts.URL+"/v1/sessions", "application/json", bytes.NewReader(createBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp2.Body)
		t.Fatalf("create session: status=%d body=%s", resp2.StatusCode, buf.String())
	}
	var sess apiTypes.CreateSessionResponse
	_ = json.NewDecoder(resp2.Body).Decode(&sess)
	if sess.Image != tag {
		t.Fatalf("session image = %q, want %q", sess.Image, tag)
	}

	// 4. Confirm the docker.CreateContainer call carried the built tag.
	if len(fc.createdSpecs) != 1 || fc.createdSpecs[0].Image != tag {
		t.Fatalf("created specs = %+v", fc.createdSpecs)
	}
	if len(fc.builtTags) != 1 || fc.builtTags[0] != tag {
		t.Fatalf("built tags = %v, want [%s]", fc.builtTags, tag)
	}
}

func TestBuildLogReturns404ForUnknown(t *testing.T) {
	ts, _, _ := newImageBuildServer(t, "")
	resp, err := http.Get(ts.URL + "/v1/images/does-not-exist/log")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestBuildEndpointDisabledWhenNoManager(t *testing.T) {
	srv := NewServer(Config{
		Docker:     &fakeClient{},
		Sessions:   session.New(time.Now),
		DefaultTTL: 60 * time.Second,
		MaxTTL:     5 * time.Minute,
		Owner:      "tester",
		PID:        12345,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	body, _ := json.Marshal(apiTypes.BuildImageRequest{Template: "node22-typescript"})
	resp, err := http.Post(ts.URL+"/v1/images/build", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}
