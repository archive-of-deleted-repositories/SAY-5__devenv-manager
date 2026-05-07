package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/SAY-5/devenv-manager/internal/docker"
	"github.com/SAY-5/devenv-manager/internal/session"
	apiTypes "github.com/SAY-5/devenv-manager/pkg/api"
)

type fakeClient struct {
	mu           sync.Mutex
	pingErr      error
	createErr    error
	startErr     error
	createdSpecs []docker.ContainerSpec
	stopped      []string
	removed      []string
	inspectFn    func(string) (docker.ContainerInfo, error)
}

func (f *fakeClient) Ping(ctx context.Context) (string, error) { return "1.45", f.pingErr }
func (f *fakeClient) CreateContainer(ctx context.Context, spec docker.ContainerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	f.createdSpecs = append(f.createdSpecs, spec)
	return "container-" + spec.ID, nil
}
func (f *fakeClient) StartContainer(ctx context.Context, id string) error { return f.startErr }
func (f *fakeClient) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, id)
	return nil
}
func (f *fakeClient) RemoveContainer(ctx context.Context, id string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	return nil
}
func (f *fakeClient) InspectContainer(ctx context.Context, id string) (docker.ContainerInfo, error) {
	if f.inspectFn != nil {
		return f.inspectFn(id)
	}
	return docker.ContainerInfo{ID: id, State: "running"}, nil
}
func (f *fakeClient) ListLabeledContainers(ctx context.Context) ([]docker.ContainerInfo, error) {
	return nil, nil
}
func (f *fakeClient) ExecAttach(ctx context.Context, id string, cmd []string, tty bool) (docker.HijackedConn, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeClient) ResizeExec(ctx context.Context, id string, cols, rows uint16) error { return nil }
func (f *fakeClient) Events(ctx context.Context, since time.Time) (<-chan docker.EventMessage, <-chan error) {
	out := make(chan docker.EventMessage)
	errs := make(chan error)
	close(out)
	close(errs)
	return out, errs
}
func (f *fakeClient) Close() error { return nil }

func newTestServer(t *testing.T, fc *fakeClient) (*httptest.Server, *session.Manager) {
	t.Helper()
	mgr := session.New(time.Now)
	srv := NewServer(Config{
		Docker:     fc,
		Sessions:   mgr,
		DefaultTTL: 60 * time.Second,
		MaxTTL:     5 * time.Minute,
		Owner:      "tester",
		PID:        12345,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, mgr
}

func TestHealthOK(t *testing.T) {
	fc := &fakeClient{}
	ts, _ := newTestServer(t, fc)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body apiTypes.HealthResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Status != "ok" {
		t.Fatalf("status field = %q", body.Status)
	}
	if rid := resp.Header.Get("X-Request-ID"); rid == "" {
		t.Fatal("X-Request-ID missing")
	}
}

func TestHealthDockerDown(t *testing.T) {
	fc := &fakeClient{pingErr: errors.New("daemon offline")}
	ts, _ := newTestServer(t, fc)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body apiTypes.ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Code != "docker_unreachable" {
		t.Fatalf("code = %q", body.Code)
	}
	if !body.Retryable {
		t.Fatal("expected retryable=true for daemon issues")
	}
}

func TestImagesAllowlist(t *testing.T) {
	fc := &fakeClient{}
	ts, _ := newTestServer(t, fc)
	resp, err := http.Get(ts.URL + "/v1/images")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body apiTypes.ImagesResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if len(body.Images) == 0 {
		t.Fatal("expected non-empty allowlist")
	}
	wanted := map[string]bool{}
	for _, img := range body.Images {
		wanted[img] = true
	}
	if !wanted["alpine:3.20"] || !wanted["python:3.12-slim"] {
		t.Fatalf("missing canonical images: %v", body.Images)
	}
}

func TestCreateSessionRejectsDisallowedImage(t *testing.T) {
	fc := &fakeClient{}
	ts, _ := newTestServer(t, fc)
	body, _ := json.Marshal(apiTypes.CreateSessionRequest{Image: "evil:latest"})
	resp, err := http.Post(ts.URL+"/v1/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var er apiTypes.ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&er)
	if er.Code != "image_not_allowed" {
		t.Fatalf("code = %q", er.Code)
	}
}

func TestCreateSessionHappyPath(t *testing.T) {
	fc := &fakeClient{}
	ts, mgr := newTestServer(t, fc)
	body, _ := json.Marshal(apiTypes.CreateSessionRequest{Image: "alpine:3.20", TTLSeconds: 30})
	resp, err := http.Post(ts.URL+"/v1/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out apiTypes.CreateSessionResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.ID == "" || out.ContainerID == "" {
		t.Fatalf("missing ids in response: %+v", out)
	}
	if got, _ := mgr.Get(out.ID); got == nil {
		t.Fatal("session not added to manager")
	}
	// Verify TTL clamping bound was honored — ttl=30 should expire ~30s out.
	delta := time.Until(out.ExpiresAt)
	if delta < 25*time.Second || delta > 35*time.Second {
		t.Fatalf("expires_at delta = %v, expected ~30s", delta)
	}
	// Verify spec contained the labeled creator PID.
	if len(fc.createdSpecs) != 1 || fc.createdSpecs[0].CreatedBy != 12345 {
		t.Fatalf("spec.CreatedBy missing pid: %+v", fc.createdSpecs)
	}
}

func TestCreateSessionTTLClampedAtMax(t *testing.T) {
	fc := &fakeClient{}
	ts, _ := newTestServer(t, fc)
	body, _ := json.Marshal(apiTypes.CreateSessionRequest{Image: "alpine:3.20", TTLSeconds: 999999})
	resp, err := http.Post(ts.URL+"/v1/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out apiTypes.CreateSessionResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	delta := time.Until(out.ExpiresAt)
	if delta > 5*time.Minute+5*time.Second {
		t.Fatalf("ttl not clamped to 5m: %v", delta)
	}
}

func TestRefreshExtendsTTL(t *testing.T) {
	fc := &fakeClient{}
	ts, mgr := newTestServer(t, fc)
	mgr.Add(&session.Session{ID: "s1", ContainerID: "c1", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(5 * time.Second)})
	resp, err := http.Post(ts.URL+"/v1/sessions/s1/refresh", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out apiTypes.RefreshResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if time.Until(out.ExpiresAt) < 50*time.Second {
		t.Fatalf("expected ~60s extension, got %v", time.Until(out.ExpiresAt))
	}
}

func TestRefreshUnknown(t *testing.T) {
	fc := &fakeClient{}
	ts, _ := newTestServer(t, fc)
	resp, err := http.Post(ts.URL+"/v1/sessions/unknown/refresh", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestDeleteSession(t *testing.T) {
	fc := &fakeClient{}
	ts, mgr := newTestServer(t, fc)
	mgr.Add(&session.Session{ID: "s1", ContainerID: "c1"})
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/sessions/s1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if _, err := mgr.Get("s1"); err == nil {
		t.Fatal("session still present after delete")
	}
	if len(fc.stopped) != 1 || len(fc.removed) != 1 {
		t.Fatalf("expected stop+remove: stopped=%v removed=%v", fc.stopped, fc.removed)
	}
}

func TestListSessions(t *testing.T) {
	fc := &fakeClient{}
	ts, mgr := newTestServer(t, fc)
	mgr.Add(&session.Session{ID: "s1", ContainerID: "c1", Image: "alpine:3.20", CreatedAt: time.Now()})
	resp, err := http.Get(ts.URL + "/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body apiTypes.ListSessionsResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if len(body.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(body.Sessions))
	}
	if body.Sessions[0].ContainerState != "running" {
		t.Fatalf("expected state running, got %q", body.Sessions[0].ContainerState)
	}
}
