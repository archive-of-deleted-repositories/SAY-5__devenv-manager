// Package ws implements the WebSocket terminal relay. The protocol is
// JSON-framed; see pkg/api for frame definitions.
//
// Backpressure design: a bounded buffered channel sits between the docker
// exec read loop and the WebSocket write loop. If the WS write side falls
// behind (slow client, congested network), output frames are dropped and
// a counter is incremented. Input frames are never dropped — stdin must
// be lossless.
package ws

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/SAY-5/devenv-manager/internal/docker"
	"github.com/SAY-5/devenv-manager/internal/session"
	"github.com/SAY-5/devenv-manager/pkg/api"
	"github.com/coder/websocket"
)

// OutputBuffer is the per-session bounded channel between Docker exec
// stdout/stderr and the WS write loop.
const OutputBuffer = 256

// Relay wires a single WebSocket connection to a Docker exec session.
type Relay struct {
	Conn       *websocket.Conn
	Session    *session.Session
	Manager    *session.Manager
	Docker     docker.Client
	TTL        time.Duration
	Logger     *slog.Logger
	OnDie      func() // called when container dies; relay closes
	DroppedOut *atomic.Int64
}

// Run drives the relay until the WS or the exec stream closes.
func (r *Relay) Run(ctx context.Context) error {
	if r.Logger == nil {
		r.Logger = slog.Default()
	}
	if r.DroppedOut == nil {
		r.DroppedOut = &atomic.Int64{}
	}

	exec, err := r.Docker.ExecAttach(ctx, r.Session.ContainerID, []string{"/bin/sh"}, true)
	if err != nil {
		_ = r.Conn.Close(websocket.StatusInternalError, "exec attach failed")
		return fmt.Errorf("ws: exec attach: %w", err)
	}
	defer func() { _ = exec.Close() }()

	out := make(chan api.Frame, OutputBuffer)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Reader: docker exec stdout -> out chan.
	go r.pumpExecToChan(ctx, exec, out, cancel)

	// Writer: out chan -> websocket.
	go r.pumpChanToWS(ctx, out, cancel)

	// Container event watcher (die propagation).
	go r.watchDie(ctx, cancel)

	// Reader: websocket -> docker exec stdin.
	return r.pumpWSToExec(ctx, exec)
}

func (r *Relay) pumpExecToChan(ctx context.Context, exec docker.HijackedConn, out chan<- api.Frame, cancel context.CancelFunc) {
	defer close(out)
	defer cancel()
	buf := make([]byte, 4096)
	for {
		n, err := exec.Read(buf)
		if n > 0 {
			frame := api.Frame{
				Type:   api.FrameOutput,
				Data:   base64.StdEncoding.EncodeToString(buf[:n]),
				Stream: "stdout",
			}
			select {
			case out <- frame:
			default:
				// Backpressure drop. Output may drop; input never does.
				r.DroppedOut.Add(1)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
				r.Logger.Debug("ws: exec read", "err", err)
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (r *Relay) pumpChanToWS(ctx context.Context, in <-chan api.Frame, cancel context.CancelFunc) {
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-in:
			if !ok {
				return
			}
			data, err := json.Marshal(frame)
			if err != nil {
				continue
			}
			wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
			err = r.Conn.Write(wctx, websocket.MessageText, data)
			wcancel()
			if err != nil {
				return
			}
		}
	}
}

func (r *Relay) pumpWSToExec(ctx context.Context, exec docker.HijackedConn) error {
	for {
		_, data, err := r.Conn.Read(ctx)
		if err != nil {
			return nil
		}
		var frame api.Frame
		if jerr := json.Unmarshal(data, &frame); jerr != nil {
			continue
		}
		switch frame.Type {
		case api.FrameInput:
			payload, derr := base64.StdEncoding.DecodeString(frame.Data)
			if derr != nil {
				continue
			}
			if _, werr := exec.Write(payload); werr != nil {
				return nil
			}
		case api.FrameResize:
			_ = r.Docker.ResizeExec(ctx, exec.ExecID(), frame.Cols, frame.Rows)
		case api.FramePing:
			if _, err := r.Manager.Refresh(r.Session.ID, r.TTL); err != nil {
				r.Logger.Debug("ws: refresh", "err", err)
			}
			pong, _ := json.Marshal(api.Frame{Type: api.FramePong})
			wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
			_ = r.Conn.Write(wctx, websocket.MessageText, pong)
			wcancel()
		case api.FrameClose:
			_ = r.Conn.Close(websocket.StatusNormalClosure, frame.Reason)
			return nil
		}
	}
}

// watchDie listens for "die" events on the underlying container and
// shuts the relay down so the WS handler doesn't leak.
func (r *Relay) watchDie(ctx context.Context, cancel context.CancelFunc) {
	events, errs := r.Docker.Events(ctx, time.Now().Add(-time.Second))
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.ContainerID == r.Session.ContainerID && ev.Action == "die" {
				if r.OnDie != nil {
					r.OnDie()
				}
				_ = r.Conn.Close(websocket.StatusNormalClosure, "container exited")
				cancel()
				return
			}
		case <-errs:
			return
		}
	}
}
