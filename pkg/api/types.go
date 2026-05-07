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
	// Volumes requests one or more named volumes to be created and
	// mounted at session start. Each volume becomes a Docker named
	// volume `devenv-{session_id}-{name}` and survives session reaping
	// (separate retention from the container itself).
	Volumes []VolumeSpec `json:"volumes,omitempty"`
	// AttachVolumeIDs lists existing volume IDs to mount on the new
	// session's container. Volumes must already have a recorded
	// MountPath; the session will mount them at that path.
	AttachVolumeIDs []string `json:"attach_volume_ids,omitempty"`
}

// VolumeSpec is the per-volume input to a session create or attach call.
type VolumeSpec struct {
	Name      string `json:"name"`
	MountPath string `json:"mount_path"`
}

// Volume is the manager-level view of a named volume.
type Volume struct {
	ID             string    `json:"id"`
	SessionID      string    `json:"session_id"`
	Name           string    `json:"name"`
	DockerVolumeID string    `json:"docker_volume_id"`
	MountPath      string    `json:"mount_path"`
	RetentionUntil time.Time `json:"retention_until"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
}

// VolumeStatus is the lifecycle stage of a managed volume.
const (
	VolumeStatusActive   = "active"
	VolumeStatusReleased = "released"
	VolumeStatusGCed     = "gced"
)

// ListVolumesResponse is returned from GET /v1/volumes.
type ListVolumesResponse struct {
	Volumes []Volume `json:"volumes"`
}

// AttachVolumesRequest is the body of POST /v1/sessions/{id}/attach-volumes.
type AttachVolumesRequest struct {
	VolumeIDs []string `json:"volume_ids"`
	// Optional override of the session config. Defaults to the parent
	// session's image/ttl when omitted.
	Image      string `json:"image,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

// VolumeGCResponse is returned from POST /v1/admin/volumes/gc.
type VolumeGCResponse struct {
	Removed []string `json:"removed_volume_ids"`
	Errors  []string `json:"errors,omitempty"`
}

// BuildImageRequest is the body of POST /v1/images/build. Template is
// the canonical name of a committed Dockerfile template (e.g.
// "node22-typescript"); arbitrary Dockerfiles are not accepted by
// design — that is the security boundary of this endpoint.
type BuildImageRequest struct {
	Template  string            `json:"template"`
	BuildArgs map[string]string `json:"build_args,omitempty"`
}

// ImageBuildStatus is the lifecycle stage of an image build.
const (
	ImageBuildStatusQueued   = "queued"
	ImageBuildStatusBuilding = "building"
	ImageBuildStatusReady    = "ready"
	ImageBuildStatusFailed   = "failed"
)

// BuildImageResponse is returned from POST /v1/images/build and from
// GET /v1/images/{id}.
type BuildImageResponse struct {
	ID           string    `json:"id"`
	Template     string    `json:"template"`
	Status       string    `json:"status"`
	ImageRef     string    `json:"image_ref,omitempty"`
	BuildLogURL  string    `json:"build_log_url"`
	CreatedAt    time.Time `json:"created_at"`
	CompletedAt  time.Time `json:"completed_at,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
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
