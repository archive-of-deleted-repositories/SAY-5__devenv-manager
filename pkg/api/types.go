// Package api defines the wire-level request and response types exchanged
// between the devenv-manager server and its clients (REST + WebSocket).
package api

import "time"

// CreateSessionRequest is the body of POST /v1/sessions.
type CreateSessionRequest struct {
	Image      string            `json:"image"`
	Env        map[string]string `json:"env,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Command    []string          `json:"command,omitempty"`
	TTLSeconds int               `json:"ttl_seconds,omitempty"`
}

// CreateSessionResponse is returned from POST /v1/sessions.
type CreateSessionResponse struct {
	ID          string    `json:"id"`
	ContainerID string    `json:"container_id"`
	WSURL       string    `json:"ws_url"`
	ExpiresAt   time.Time `json:"expires_at"`
	Image       string    `json:"image"`
}

// Session is the listing/detail view of a session.
type Session struct {
	ID             string    `json:"id"`
	Image          string    `json:"image"`
	ContainerID    string    `json:"container_id"`
	ContainerState string    `json:"container_state"`
	CreatedAt      time.Time `json:"created_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	WorkingDir     string    `json:"working_dir,omitempty"`
}

// ListSessionsResponse is returned from GET /v1/sessions.
type ListSessionsResponse struct {
	Sessions   []Session `json:"sessions"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

// RefreshResponse is returned from POST /v1/sessions/{id}/refresh.
type RefreshResponse struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ImagesResponse is returned from GET /v1/images.
type ImagesResponse struct {
	Images []string `json:"images"`
}

// HealthResponse is returned from GET /healthz.
type HealthResponse struct {
	Status        string `json:"status"`
	DockerVersion string `json:"docker_version,omitempty"`
}

// ErrorResponse is the canonical error envelope.
type ErrorResponse struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	RequestID string `json:"request_id,omitempty"`
}

// WebSocket frame types.
const (
	FrameInput  = "input"
	FrameOutput = "output"
	FrameResize = "resize"
	FramePing   = "ping"
	FramePong   = "pong"
	FrameClose  = "close"
)

// Frame is the JSON envelope used over the WebSocket.
type Frame struct {
	Type   string `json:"type"`
	Data   string `json:"data,omitempty"`   // base64
	Stream string `json:"stream,omitempty"` // "stdout" | "stderr"
	Cols   uint16 `json:"cols,omitempty"`
	Rows   uint16 `json:"rows,omitempty"`
	Reason string `json:"reason,omitempty"`
}
