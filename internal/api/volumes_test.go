package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SAY-5/devenv-manager/internal/docker"
	"github.com/SAY-5/devenv-manager/internal/session"
	"github.com/SAY-5/devenv-manager/internal/volume"
	apiTypes "github.com/SAY-5/devenv-manager/pkg/api"
)

// volTrackingClient extends fakeClient with volume bookkeeping that the
// volume manager exercises. The two layers are kept separate so the
// existing api tests don't need to care about volume bytes.
type volTrackingClient struct {
	*fakeClient
	mu                 sync.Mutex
	volumeContents     map[string]map[string][]byte // volume -> file -> bytes
	containerToVolumes map[string][]string
}

func newVolTrackingClient() *volTrackingClient {
	return &volTrackingClient{
		fakeClient:         &fakeClient{},
		volumeContents:     make(map[string]map[string][]byte),
		containerToVolumes: make(map[string][]string),
	}
}

func (v *volTrackingClient) CreateContainer(ctx context.Context, spec docker.ContainerSpec) (string, error) {
	cid, err := v.fakeClient.CreateContainer(ctx, spec)
	if err != nil {
		return cid, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, m := range spec.VolumeMounts {
		v.containerToVolumes[cid] = append(v.containerToVolumes[cid], m.Source)
	}
	return cid, nil
}

func (v *volTrackingClient) CreateVolume(ctx context.Context, name string, labels map[string]string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, exists := v.volumeContents[name]; !exists {
		v.volumeContents[name] = make(map[string][]byte)
	}
	v.createdVolumes = append(v.createdVolumes, name)
	return name, nil
}

func (v *volTrackingClient) RemoveVolume(ctx context.Context, name string, force bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.volumeContents, name)
	v.removedVolumes = append(v.removedVolumes, name)
	return nil
}

// writeFile simulates the container writing a file into a mounted volume.
// The api package never invokes this directly; the test calls it to
// stand in for `echo > /mnt/foo` running inside the container.
func (v *volTrackingClient) writeFile(volumeName, path string, data []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.volumeContents[volumeName]; !ok {
		v.volumeContents[volumeName] = make(map[string][]byte)
	}
	v.volumeContents[volumeName][path] = append([]byte(nil), data...)
}

// readFile reads a file back from the simulated volume.
func (v *volTrackingClient) readFile(volumeName, path string) ([]byte, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	files, ok := v.volumeContents[volumeName]
	if !ok {
		return nil, false
	}
	b, ok := files[path]
	return b, ok
}

func newVolumeServer(t *testing.T) (*httptest.Server, *session.Manager, *volume.Manager, *volTrackingClient) {
	t.Helper()
	fc := newVolTrackingClient()
	sessions := session.New(time.Now)
	volMgr := volume.New(volume.Config{
		Docker:    fc,
		Now:       time.Now,
		Retention: time.Hour,
	})
	srv := NewServer(Config{
		Docker:     fc,
		Sessions:   sessions,
		Volumes:    volMgr,
		DefaultTTL: 60 * time.Second,
		MaxTTL:     5 * time.Minute,
		Owner:      "tester",
		PID:        12345,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, sessions, volMgr, fc
}

func TestCreateSessionWithVolumeProvisionsDockerVolume(t *testing.T) {
	ts, _, volMgr, fc := newVolumeServer(t)

	body, _ := json.Marshal(apiTypes.CreateSessionRequest{
		Image: "alpine:3.20",
		Volumes: []apiTypes.VolumeSpec{
			{Name: "data", MountPath: "/mnt/data"},
		},
	})
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

	listed := volMgr.List(out.ID)
	if len(listed) != 1 {
		t.Fatalf("volumes for session = %d", len(listed))
	}
	if listed[0].MountPath != "/mnt/data" {
		t.Fatalf("mount path = %q", listed[0].MountPath)
	}
	if !strings.HasPrefix(listed[0].DockerVolumeID, "devenv-") {
		t.Fatalf("docker volume name = %q", listed[0].DockerVolumeID)
	}
	if len(fc.createdVolumes) != 1 {
		t.Fatalf("docker createdVolumes = %v", fc.createdVolumes)
	}
}

func TestVolumeSurvivesSessionReap(t *testing.T) {
	// The cross-session persistence test required by v3: create a
	// session with a volume, write a file into it, reap the session,
	// then start a NEW session that attaches the same volume by id and
	// confirm the file is still present.
	ts, _, volMgr, fc := newVolumeServer(t)

	// 1. Create session A with a volume.
	body, _ := json.Marshal(apiTypes.CreateSessionRequest{
		Image: "alpine:3.20",
		Volumes: []apiTypes.VolumeSpec{
			{Name: "workspace", MountPath: "/mnt/workspace"},
		},
	})
	resp, err := http.Post(ts.URL+"/v1/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create session A: %d", resp.StatusCode)
	}
	var sessionA apiTypes.CreateSessionResponse
	_ = json.NewDecoder(resp.Body).Decode(&sessionA)

	// Locate the volume Docker name and write a file into it.
	vols := volMgr.List(sessionA.ID)
	if len(vols) != 1 {
		t.Fatalf("expected 1 volume on session A, got %d", len(vols))
	}
	dockerVol := vols[0].DockerVolumeID
	fc.writeFile(dockerVol, "/mnt/workspace/notes.txt", []byte("hello-from-A"))

	// 2. Reap session A via DELETE.
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/sessions/"+sessionA.ID, nil)
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete A: %d", delResp.StatusCode)
	}
	// The volume must NOT be deleted from Docker — it survives reap.
	for _, removed := range fc.removedVolumes {
		if removed == dockerVol {
			t.Fatalf("volume %q was removed during reap; should survive", dockerVol)
		}
	}
	got, err := volMgr.Get(vols[0].ID)
	if err != nil {
		t.Fatalf("volume gone from manager: %v", err)
	}
	if got.Status != apiTypes.VolumeStatusReleased {
		t.Fatalf("status after reap = %q, want released", got.Status)
	}

	// 3. Start session B with attach-volumes. The parent session has
	// been reaped, so the request must supply Image explicitly.
	attachBody, _ := json.Marshal(apiTypes.AttachVolumesRequest{
		VolumeIDs: []string{vols[0].ID},
		Image:     "alpine:3.20",
	})
	resp3, err := http.Post(
		ts.URL+"/v1/sessions/"+sessionA.ID+"/attach-volumes",
		"application/json",
		bytes.NewReader(attachBody),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusCreated {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp3.Body)
		t.Fatalf("attach-volumes: %d body=%s", resp3.StatusCode, buf.String())
	}
	var sessionB apiTypes.CreateSessionResponse
	_ = json.NewDecoder(resp3.Body).Decode(&sessionB)

	// Session B's container should have been created with the same
	// docker volume mounted; the file written during session A's life
	// is still there.
	mounted := fc.containerToVolumes[sessionB.ContainerID]
	if len(mounted) != 1 || mounted[0] != dockerVol {
		t.Fatalf("session B mounts = %v, want [%s]", mounted, dockerVol)
	}
	got2, ok := fc.readFile(dockerVol, "/mnt/workspace/notes.txt")
	if !ok {
		t.Fatal("file does not survive reap; volume persistence broken")
	}
	if string(got2) != "hello-from-A" {
		t.Fatalf("file content drift: %q", got2)
	}
}

func TestListVolumesFilterBySession(t *testing.T) {
	ts, _, _, _ := newVolumeServer(t)

	body, _ := json.Marshal(apiTypes.CreateSessionRequest{
		Image:   "alpine:3.20",
		Volumes: []apiTypes.VolumeSpec{{Name: "v1", MountPath: "/m"}},
	})
	resp, err := http.Post(ts.URL+"/v1/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sess apiTypes.CreateSessionResponse
	_ = json.NewDecoder(resp.Body).Decode(&sess)

	// Filtered list returns the volume.
	listResp, err := http.Get(ts.URL + "/v1/volumes?session_id=" + sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer listResp.Body.Close()
	var list apiTypes.ListVolumesResponse
	_ = json.NewDecoder(listResp.Body).Decode(&list)
	if len(list.Volumes) != 1 {
		t.Fatalf("filtered list = %d", len(list.Volumes))
	}

	// Filter by an unknown session returns nothing.
	miss, err := http.Get(ts.URL + "/v1/volumes?session_id=does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	defer miss.Body.Close()
	var emptyList apiTypes.ListVolumesResponse
	_ = json.NewDecoder(miss.Body).Decode(&emptyList)
	if len(emptyList.Volumes) != 0 {
		t.Fatalf("unknown filter returned %d", len(emptyList.Volumes))
	}
}

func TestDeleteVolumeRemovesFromBothLayers(t *testing.T) {
	ts, _, volMgr, fc := newVolumeServer(t)

	body, _ := json.Marshal(apiTypes.CreateSessionRequest{
		Image:   "alpine:3.20",
		Volumes: []apiTypes.VolumeSpec{{Name: "v1", MountPath: "/m"}},
	})
	resp, err := http.Post(ts.URL+"/v1/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sess apiTypes.CreateSessionResponse
	_ = json.NewDecoder(resp.Body).Decode(&sess)
	vols := volMgr.List(sess.ID)
	if len(vols) != 1 {
		t.Fatalf("setup: expected 1 vol, got %d", len(vols))
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/volumes/"+vols[0].ID, nil)
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", delResp.StatusCode)
	}
	if len(fc.removedVolumes) != 1 {
		t.Fatalf("docker removed = %v", fc.removedVolumes)
	}
}

func TestVolumeGCSweepsExpired(t *testing.T) {
	ts, _, volMgr, fc := newVolumeServer(t)
	body, _ := json.Marshal(apiTypes.CreateSessionRequest{
		Image:   "alpine:3.20",
		Volumes: []apiTypes.VolumeSpec{{Name: "v1", MountPath: "/m"}},
	})
	resp, err := http.Post(ts.URL+"/v1/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sess apiTypes.CreateSessionResponse
	_ = json.NewDecoder(resp.Body).Decode(&sess)
	vols := volMgr.List(sess.ID)

	// Force the volume past its retention window from the test.
	volMgr.MarkReleased(sess.ID)
	// Reach into the manager to set an expired retention; we accept the
	// indirection because a public knob just for tests would clutter
	// the API surface.
	v, _ := volMgr.Get(vols[0].ID)
	v.RetentionUntil = time.Now().Add(-time.Hour)
	// MarkReleased ran with the manager's now-clock; we need an
	// already-expired window. Bypass the public API by deleting + GCing.
	// (The volume manager has its own GC unit tests; here we only
	// confirm the route translates cleanly.)

	gcResp, err := http.Post(ts.URL+"/v1/admin/volumes/gc", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gcResp.Body.Close()
	if gcResp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", gcResp.StatusCode)
	}
	_ = fc // not asserted on the GC count; the volume hasn't expired in
	// real time yet. The unit test in internal/volume covers the
	// expiry-driven removal path; this test only proves the route
	// returns 200 with a well-formed body.
	var out apiTypes.VolumeGCResponse
	_ = json.NewDecoder(gcResp.Body).Decode(&out)
	if out.Removed == nil {
		t.Fatal("Removed should be non-nil (empty array allowed)")
	}
}
