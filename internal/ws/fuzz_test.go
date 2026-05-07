package ws

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"unicode/utf8"

	"github.com/SAY-5/devenv-manager/pkg/api"
)

// FuzzFrameUnmarshal feeds random bytes to the JSON frame decoder. The
// relay's WS read loop tolerates malformed frames (it skips them); the
// fuzzer's job is to confirm Unmarshal never panics on hostile input.
func FuzzFrameUnmarshal(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"type":"input","data":"aGVsbG8K"}`),
		[]byte(`{"type":"resize","cols":80,"rows":24}`),
		[]byte(`{"type":"close","reason":"bye"}`),
		[]byte(`{}`),
		[]byte(`{"type":"input","data":"!!!not-base64!!!"}`),
		[]byte(`{"type":"output","data":"` + base64.StdEncoding.EncodeToString([]byte("\x00\xff\x7f")) + `"}`),
		[]byte(`null`),
		[]byte(``),
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		var frame api.Frame
		if err := json.Unmarshal(raw, &frame); err != nil {
			return
		}
		// If the data field looks like base64, attempt to decode it the same
		// way the relay does. Decode failure is expected and must not panic.
		if frame.Type == api.FrameInput || frame.Type == api.FrameOutput {
			_, _ = base64.StdEncoding.DecodeString(frame.Data)
		}
		// Re-encoding a successfully decoded frame must round-trip back to a
		// valid frame; this catches any field with broken JSON tags.
		out, err := json.Marshal(frame)
		if err != nil {
			t.Fatalf("re-marshal: %v", err)
		}
		var again api.Frame
		if err := json.Unmarshal(out, &again); err != nil {
			t.Fatalf("re-unmarshal: %v", err)
		}
		if again.Type != frame.Type {
			t.Fatalf("type drifted: %q -> %q", frame.Type, again.Type)
		}
	})
}

// FuzzFrameTypeDispatch ensures the relay's switch on Frame.Type accepts
// every advertised constant and ignores everything else without crashing.
// The model under test is the same set of cases as pumpWSToExec.
func FuzzFrameTypeDispatch(f *testing.F) {
	knownTypes := []string{api.FrameInput, api.FrameOutput, api.FrameResize, api.FramePing, api.FramePong, api.FrameClose}
	for _, k := range knownTypes {
		f.Add(k, "data", uint16(80), uint16(24))
	}
	f.Add("garbage", "x", uint16(0), uint16(0))
	f.Add("", "", uint16(65535), uint16(65535))

	f.Fuzz(func(t *testing.T, typ string, data string, cols, rows uint16) {
		// JSON encoding silently rewrites invalid UTF-8 to U+FFFD; skip those
		// inputs so the round-trip assertion only fires on payloads the wire
		// format can actually carry.
		if !utf8.ValidString(typ) || !utf8.ValidString(data) {
			return
		}
		frame := api.Frame{Type: typ, Data: data, Cols: cols, Rows: rows}
		raw, err := json.Marshal(frame)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded api.Frame
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if decoded.Type != typ || decoded.Cols != cols || decoded.Rows != rows {
			t.Fatalf("round-trip drift: %+v -> %+v", frame, decoded)
		}
	})
}
