// Command concurrent drives N parallel session-create requests against a
// running devenv-manager and records provisioning + first-byte latency
// percentiles plus manager process resource use.
//
// Usage:
//
//	go run ./bench -n=100 -addr=127.0.0.1:18080 -out=bench/results/run.json
//
// The intended workflow is to start a fresh server (the run-bench-server
// helper in the Makefile does this) and feed it N=100 sessions
// concurrently; CI runs a smaller N=10 smoke through the same code path.
//
// The latency vocabulary is:
//
//   - provision: time from "POST /v1/sessions" submission to a 201
//     response with a usable session ID.
//   - first byte: time from WebSocket open to the first inbound output
//     frame the relay forwards from the container's PID 1.
//
// Both are recorded per-session; the JSON output reports P50/P95/P99 over
// the full set, plus the failure count and timestamps for forensics.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	apiTypes "github.com/SAY-5/devenv-manager/pkg/api"
)

type sample struct {
	ID            string        `json:"id"`
	Provision     time.Duration `json:"provision_ns"`
	FirstByte     time.Duration `json:"first_byte_ns"`
	Err           string        `json:"err,omitempty"`
	OutputPreview string        `json:"output_preview,omitempty"`
}

type summary struct {
	Timestamp           time.Time         `json:"timestamp"`
	N                   int               `json:"n"`
	Concurrency         int               `json:"concurrency"`
	Image               string            `json:"image"`
	Addr                string            `json:"addr"`
	WallClock           time.Duration     `json:"wall_clock_ns"`
	Successes           int               `json:"successes"`
	Failures            int               `json:"failures"`
	ProvisionP50        time.Duration     `json:"provision_p50_ns"`
	ProvisionP95        time.Duration     `json:"provision_p95_ns"`
	ProvisionP99        time.Duration     `json:"provision_p99_ns"`
	FirstByteP50        time.Duration     `json:"first_byte_p50_ns"`
	FirstByteP95        time.Duration     `json:"first_byte_p95_ns"`
	FirstByteP99        time.Duration     `json:"first_byte_p99_ns"`
	ManagerCPUDelta     float64           `json:"manager_cpu_delta_seconds,omitempty"`
	ManagerMaxRSSBytes  uint64            `json:"manager_max_rss_bytes,omitempty"`
	GoRuntimeStatsStart map[string]uint64 `json:"go_runtime_start"`
	GoRuntimeStatsEnd   map[string]uint64 `json:"go_runtime_end"`
	Samples             []sample          `json:"samples"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "manager listen address")
	n := flag.Int("n", 100, "number of sessions to launch")
	concurrency := flag.Int("concurrency", 0, "max in-flight (defaults to n)")
	image := flag.String("image", "alpine:3.20", "image to provision")
	ttl := flag.Int("ttl", 60, "session TTL seconds")
	out := flag.String("out", "", "JSON output path; default bench/results/<unix>.json")
	managerPID := flag.Int("manager-pid", 0, "optional manager pid for resource sampling")
	timeout := flag.Duration("timeout", 5*time.Minute, "wall-clock cap")
	flag.Parse()

	if *concurrency <= 0 {
		*concurrency = *n
	}
	if *out == "" {
		*out = filepath.Join("bench", "results", fmt.Sprintf("%d.json", time.Now().Unix()))
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o750); err != nil {
		fail("mkdir results: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Verify the manager is reachable before piling traffic on it.
	if err := waitHealthy(ctx, *addr); err != nil {
		fail("health gate: %v", err)
	}

	startStats := goRuntimeStats()
	managerStart := readManagerCPU(*managerPID)

	samples := make([]sample, *n)
	sem := make(chan struct{}, *concurrency)
	var wg sync.WaitGroup
	wallStart := time.Now()
	for i := 0; i < *n; i++ {
		i := i
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			samples[i] = run(ctx, *addr, *image, *ttl)
		}()
	}
	wg.Wait()
	wall := time.Since(wallStart)
	endStats := goRuntimeStats()
	managerEnd := readManagerCPU(*managerPID)

	provs := make([]time.Duration, 0, *n)
	fbs := make([]time.Duration, 0, *n)
	successes, failures := 0, 0
	for _, s := range samples {
		if s.Err != "" {
			failures++
			continue
		}
		successes++
		provs = append(provs, s.Provision)
		if s.FirstByte > 0 {
			fbs = append(fbs, s.FirstByte)
		}
	}

	sum := summary{
		Timestamp:           time.Now().UTC(),
		N:                   *n,
		Concurrency:         *concurrency,
		Image:               *image,
		Addr:                *addr,
		WallClock:           wall,
		Successes:           successes,
		Failures:            failures,
		ProvisionP50:        percentile(provs, 0.50),
		ProvisionP95:        percentile(provs, 0.95),
		ProvisionP99:        percentile(provs, 0.99),
		FirstByteP50:        percentile(fbs, 0.50),
		FirstByteP95:        percentile(fbs, 0.95),
		FirstByteP99:        percentile(fbs, 0.99),
		ManagerCPUDelta:     managerEnd.cpuSeconds - managerStart.cpuSeconds,
		ManagerMaxRSSBytes:  maxBytes(managerStart.rss, managerEnd.rss),
		GoRuntimeStatsStart: startStats,
		GoRuntimeStatsEnd:   endStats,
		Samples:             samples,
	}
	body, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		fail("marshal: %v", err)
	}
	if err := os.WriteFile(*out, body, 0o600); err != nil {
		fail("write: %v", err)
	}
	fmt.Printf("bench: wrote %s (n=%d successes=%d failures=%d wall=%s p50=%s p95=%s p99=%s)\n",
		*out, *n, successes, failures, wall, sum.ProvisionP50, sum.ProvisionP95, sum.ProvisionP99)
	if failures > 0 {
		os.Exit(1)
	}
}

func run(ctx context.Context, addr, image string, ttlSec int) sample {
	body := apiTypes.CreateSessionRequest{Image: image, TTLSeconds: ttlSec}
	raw, _ := json.Marshal(body)
	provStart := time.Now()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+addr+"/v1/sessions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return sample{Err: "create: " + err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return sample{Err: fmt.Sprintf("create http %d: %s", resp.StatusCode, b)}
	}
	var created apiTypes.CreateSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return sample{Err: "decode: " + err.Error()}
	}
	prov := time.Since(provStart)

	wsURL := "ws://" + addr + "/v1/sessions/" + created.ID + "/ws"
	wsCtx, wsCancel := context.WithTimeout(ctx, 30*time.Second)
	defer wsCancel()
	c, dialResp, err := websocket.Dial(wsCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"http://" + addr}},
	})
	if dialResp != nil && dialResp.Body != nil {
		_ = dialResp.Body.Close()
	}
	if err != nil {
		return sample{ID: created.ID, Provision: prov, Err: "ws dial: " + err.Error()}
	}
	defer func() { _ = c.Close(websocket.StatusNormalClosure, "bench done") }()

	// Send a one-shot input that produces deterministic output, then wait
	// for the first output frame and record the elapsed time.
	echoCmd := apiTypes.Frame{
		Type: apiTypes.FrameInput,
		Data: base64.StdEncoding.EncodeToString([]byte("printf bench-ok\\n\n")),
	}
	cmdRaw, _ := json.Marshal(echoCmd)
	if err := c.Write(wsCtx, websocket.MessageText, cmdRaw); err != nil {
		return sample{ID: created.ID, Provision: prov, Err: "ws write: " + err.Error()}
	}

	fbStart := time.Now()
	deadline := time.Now().Add(20 * time.Second)
	preview := ""
	for time.Now().Before(deadline) {
		_, data, err := c.Read(wsCtx)
		if err != nil {
			return sample{ID: created.ID, Provision: prov, Err: "ws read: " + err.Error()}
		}
		var f apiTypes.Frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		if f.Type != apiTypes.FrameOutput {
			continue
		}
		raw, derr := base64.StdEncoding.DecodeString(f.Data)
		if derr != nil {
			continue
		}
		preview = strings.TrimSpace(string(raw))
		// Skip the prompt-only line ("/ #") and wait for the actual echo.
		if strings.Contains(preview, "bench-ok") || len(preview) > 0 {
			return sample{
				ID:            created.ID,
				Provision:     prov,
				FirstByte:     time.Since(fbStart),
				OutputPreview: truncate(preview, 80),
			}
		}
	}
	return sample{ID: created.ID, Provision: prov, Err: "no output within 20s", OutputPreview: preview}
}

func waitHealthy(ctx context.Context, addr string) error {
	for i := 0; i < 50; i++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return errors.New("manager never reported healthy")
}

func percentile(xs []time.Duration, p float64) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]time.Duration(nil), xs...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(float64(len(cp)-1) * p)
	return cp[idx]
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func goRuntimeStats() map[string]uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	gor := uint64(0)
	if n := runtime.NumGoroutine(); n > 0 {
		gor = uint64(n) //nolint:gosec // bounded above by goroutine count
	}
	return map[string]uint64{
		"alloc_bytes":      m.Alloc,
		"total_alloc":      m.TotalAlloc,
		"sys_bytes":        m.Sys,
		"num_gc":           uint64(m.NumGC),
		"goroutines":       gor,
		"heap_objects":     m.HeapObjects,
		"heap_inuse_bytes": m.HeapInuse,
	}
}

type managerSample struct {
	cpuSeconds float64
	rss        uint64
}

// readManagerCPU pulls the cumulative CPU time and RSS for the manager
// process via `ps` so we don't need a privileged debug endpoint. Returns
// zeros if pid==0 or the process can't be probed. The `time=` column is
// formatted as `[D-]HH:MM:SS` on Linux/macOS and is the canonical
// portable cumulative-CPU readout.
func readManagerCPU(pid int) managerSample {
	if pid <= 0 {
		return managerSample{}
	}
	cmd := exec.Command("ps", "-o", "time=,rss=", "-p", fmt.Sprint(pid)) // #nosec G204
	b, err := cmd.Output()
	if err != nil {
		return managerSample{}
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 {
		return managerSample{}
	}
	cpu := parsePsTime(fields[0])
	var rss uint64
	_, _ = fmt.Sscanf(fields[1], "%d", &rss)
	return managerSample{cpuSeconds: cpu, rss: rss * 1024} // ps reports rss in KiB
}

// parsePsTime parses ps's `time=` column into seconds. Format is one of:
//
//	HH:MM:SS
//	MM:SS
//	D-HH:MM:SS  (only seen for processes alive across day boundaries)
func parsePsTime(s string) float64 {
	s = strings.TrimSpace(s)
	dayPart := 0.0
	if dash := strings.IndexByte(s, '-'); dash >= 0 {
		var d int
		_, _ = fmt.Sscanf(s[:dash], "%d", &d)
		dayPart = float64(d) * 86400
		s = s[dash+1:]
	}
	parts := strings.Split(s, ":")
	mult := []float64{1, 60, 3600}
	total := 0.0
	for i := 0; i < len(parts) && i < 3; i++ {
		var v float64
		_, _ = fmt.Sscanf(parts[len(parts)-1-i], "%f", &v)
		total += v * mult[i]
	}
	return dayPart + total
}

func maxBytes(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(2)
}
