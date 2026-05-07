package ws

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/SAY-5/devenv-manager/pkg/api"
)

func TestFrameRoundTrip(t *testing.T) {
	original := []byte("hello\n")
	encoded := base64.StdEncoding.EncodeToString(original)
	frame := api.Frame{Type: api.FrameInput, Data: encoded}

	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded api.Frame
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Type != api.FrameInput {
		t.Fatalf("type = %q", decoded.Type)
	}

	got, err := base64.StdEncoding.DecodeString(decoded.Data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(got) != "hello\n" {
		t.Fatalf("payload = %q", got)
	}
}

func TestResizeFrameSerialization(t *testing.T) {
	frame := api.Frame{Type: api.FrameResize, Cols: 80, Rows: 24}
	raw, _ := json.Marshal(frame)
	var decoded api.Frame
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Cols != 80 || decoded.Rows != 24 {
		t.Fatalf("cols=%d rows=%d", decoded.Cols, decoded.Rows)
	}
}

func TestOutputBufferConstantIs256(t *testing.T) {
	if OutputBuffer != 256 {
		t.Fatalf("OutputBuffer = %d, want 256 (documented backpressure budget)", OutputBuffer)
	}
}
