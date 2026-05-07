package ws

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SAY-5/devenv-manager/internal/docker"
	"github.com/SAY-5/devenv-manager/internal/session"
	"github.com/SAY-5/devenv-manager/pkg/api"
	"github.com/coder/websocket"
)

// loopbackConn is a paired io.ReadWriteCloser used as a fake exec attach.
type loopbackConn struct {
	mu       sync.Mutex
	readBuf  chan []byte
	writeBuf [][]byte
	closed   bool
}

func newLoopback() *loopbackConn { return &loopbackConn{readBuf: make(chan []byte, 16)} }

func (l *loopbackConn) Read(p []byte) (int, error) {
	b, ok := <-l.readBuf
	if !ok {
		return 0, io.EOF
	}
	n := copy(p, b)
	return n, nil
}
func (l *loopbackConn) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, errors.New("closed")
	}
	l.writeBuf = append(l.writeBuf, append([]byte(nil), p...))
	return len(p), nil
}
func (l *loopbackConn) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	close(l.readBuf)
	return nil
}
func (l *loopbackConn) ExecID() string { return "exec-test" }

func (l *loopbackConn) writes() [][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([][]byte(nil), l.writeBuf...)
}

// fakeDocker is a Client that returns the loopbackConn from ExecAttach.
type fakeDocker struct {
	conn     *loopbackConn
	resizes  chan struct{ cols, rows uint16 }
	eventsCh chan docker.EventMessage
	errsCh   chan error
}

func (f *fakeDocker) Ping(ctx context.Context) (string, error) { return "test", nil }
func (f *fakeDocker) CreateContainer(ctx context.Context, spec docker.ContainerSpec) (string, error) {
	return "", nil
}
func (f *fakeDocker) StartContainer(ctx context.Context, id string) error { return nil }
func (f *fakeDocker) StopContainer(ctx context.Context, id string, t time.Duration) error {
	return nil
}
func (f *fakeDocker) RemoveContainer(ctx context.Context, id string, force bool) error { return nil }
func (f *fakeDocker) InspectContainer(ctx context.Context, id string) (docker.ContainerInfo, error) {
	return docker.ContainerInfo{ID: id, State: "running"}, nil
}
func (f *fakeDocker) ListLabeledContainers(ctx context.Context) ([]docker.ContainerInfo, error) {
	return nil, nil
}
func (f *fakeDocker) ExecAttach(ctx context.Context, id string, cmd []string, tty bool) (docker.HijackedConn, error) {
	return f.conn, nil
}
func (f *fakeDocker) ResizeExec(ctx context.Context, id string, cols, rows uint16) error {
	if f.resizes != nil {
		f.resizes <- struct{ cols, rows uint16 }{cols, rows}
	}
	return nil
}
func (f *fakeDocker) Events(ctx context.Context, since time.Time) (<-chan docker.EventMessage, <-chan error) {
	return f.eventsCh, f.errsCh
}
func (f *fakeDocker) CreateVolume(ctx context.Context, name string, labels map[string]string) (string, error) {
	return name, nil
}
func (f *fakeDocker) RemoveVolume(ctx context.Context, name string, force bool) error { return nil }
func (f *fakeDocker) BuildImage(ctx context.Context, spec docker.ImageBuildSpec) (io.ReadCloser, error) {
	return io.NopCloser(io.MultiReader()), nil
}
func (f *fakeDocker) Close() error { return nil }

func TestRelayPumpsOutputToWS(t *testing.T) {
	loop := newLoopback()
	defer loop.Close()
	fd := &fakeDocker{
		conn:     loop,
		eventsCh: make(chan docker.EventMessage),
		errsCh:   make(chan error),
	}
	mgr := session.New(time.Now)
	sess := mgr.Add(&session.Session{
		ID:          "s1",
		ContainerID: "c1",
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(time.Minute),
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		dropped := &atomic.Int64{}
		relay := &Relay{
			Conn:       c,
			Session:    sess,
			Manager:    mgr,
			Docker:     fd,
			TTL:        30 * time.Second,
			DroppedOut: dropped,
		}
		_ = relay.Run(r.Context())
	}))
	defer server.Close()

	wsURL := "ws" + server.URL[len("http"):]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cli, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	defer cli.Close(websocket.StatusNormalClosure, "test done")

	// Push some bytes from "exec" -> WS.
	loop.readBuf <- []byte("hello-world\n")

	_, raw, err := cli.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got api.Frame
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != api.FrameOutput {
		t.Fatalf("type = %q", got.Type)
	}
	payload, _ := base64.StdEncoding.DecodeString(got.Data)
	if string(payload) != "hello-world\n" {
		t.Fatalf("payload = %q", payload)
	}
}

func TestRelayPushesInputToExec(t *testing.T) {
	loop := newLoopback()
	defer loop.Close()
	fd := &fakeDocker{
		conn:     loop,
		eventsCh: make(chan docker.EventMessage),
		errsCh:   make(chan error),
	}
	mgr := session.New(time.Now)
	sess := mgr.Add(&session.Session{ID: "s1", ContainerID: "c1", ExpiresAt: time.Now().Add(time.Minute)})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		relay := &Relay{
			Conn: c, Session: sess, Manager: mgr, Docker: fd,
			TTL: 30 * time.Second, DroppedOut: &atomic.Int64{},
		}
		_ = relay.Run(r.Context())
	}))
	defer server.Close()

	wsURL := "ws" + server.URL[len("http"):]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cli, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	frame := api.Frame{Type: api.FrameInput, Data: base64.StdEncoding.EncodeToString([]byte("ls\n"))}
	raw, _ := json.Marshal(frame)
	if err := cli.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Wait until the loopback has seen the byte.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ws := loop.writes()
		if len(ws) > 0 {
			if string(ws[0]) != "ls\n" {
				t.Fatalf("write = %q", ws[0])
			}
			cli.Close(websocket.StatusNormalClosure, "done")
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	cli.Close(websocket.StatusNormalClosure, "done")
	t.Fatal("timeout waiting for input to reach exec")
}

func TestRelayPingRefreshesTTL(t *testing.T) {
	loop := newLoopback()
	defer loop.Close()
	fd := &fakeDocker{
		conn:     loop,
		eventsCh: make(chan docker.EventMessage),
		errsCh:   make(chan error),
	}
	now := time.Now()
	mgr := session.New(func() time.Time { return now })
	sess := mgr.Add(&session.Session{ID: "s1", ContainerID: "c1", CreatedAt: now, ExpiresAt: now.Add(5 * time.Second)})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		relay := &Relay{
			Conn: c, Session: sess, Manager: mgr, Docker: fd,
			TTL: 60 * time.Second, DroppedOut: &atomic.Int64{},
		}
		_ = relay.Run(r.Context())
	}))
	defer server.Close()

	wsURL := "ws" + server.URL[len("http"):]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cli, resp, derr := websocket.Dial(ctx, wsURL, nil)
	if derr != nil {
		t.Fatalf("dial: %v", derr)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	raw, _ := json.Marshal(api.Frame{Type: api.FramePing})
	_ = cli.Write(ctx, websocket.MessageText, raw)

	// Pong should arrive.
	deadline := time.Now().Add(2 * time.Second)
	pongSeen := false
	for time.Now().Before(deadline) && !pongSeen {
		readCtx, readCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		_, data, rerr := cli.Read(readCtx)
		readCancel()
		if rerr != nil {
			continue
		}
		var f api.Frame
		_ = json.Unmarshal(data, &f)
		if f.Type == api.FramePong {
			pongSeen = true
		}
	}
	cli.Close(websocket.StatusNormalClosure, "done")
	if !pongSeen {
		t.Fatal("did not receive pong frame")
	}
	got, _ := mgr.Get("s1")
	if got.ExpiresAt.Sub(now) < 50*time.Second {
		t.Fatalf("ExpiresAt not extended; delta=%v", got.ExpiresAt.Sub(now))
	}
}
