// Package api wires HTTP and WebSocket handlers on top of the docker,
// session, and ws packages.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sync/atomic"
	"time"

	"github.com/SAY-5/devenv-manager/internal/docker"
	"github.com/SAY-5/devenv-manager/internal/session"
	"github.com/SAY-5/devenv-manager/internal/volume"
	"github.com/SAY-5/devenv-manager/internal/ws"
	"github.com/SAY-5/devenv-manager/pkg/api"
	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// AllowedImages is the canonical list of images the server will provision.
// Anything outside this list is rejected with code "image_not_allowed".
var AllowedImages = []string{
	"alpine:3.20",
	"ubuntu:24.04",
	"node:22-alpine",
	"python:3.12-slim",
	"golang:1.22-alpine",
}

// Config configures the HTTP server.
type Config struct {
	Docker        docker.Client
	Sessions      *session.Manager
	Volumes       *volume.Manager
	DefaultTTL    time.Duration
	MaxTTL        time.Duration
	Owner         string
	Logger        *slog.Logger
	WSAllowOrigin []string // permitted Origin headers in dev
	Now           func() time.Time
	PID           int
	DroppedOut    *atomic.Int64
}

// Server bundles handler state.
type Server struct {
	cfg Config
}

// NewServer returns an http.Handler implementing the v1 API.
func NewServer(cfg Config) *Server {
	if cfg.DefaultTTL == 0 {
		cfg.DefaultTTL = 60 * time.Second
	}
	if cfg.MaxTTL == 0 {
		cfg.MaxTTL = 30 * time.Minute
	}
	if cfg.Owner == "" {
		cfg.Owner = "anonymous"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PID == 0 {
		cfg.PID = os.Getpid()
	}
	if cfg.DroppedOut == nil {
		cfg.DroppedOut = &atomic.Int64{}
	}
	return &Server{cfg: cfg}
}

// Handler returns the http.Handler with all v1 routes mounted.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /v1/images", s.handleImages)
	mux.HandleFunc("POST /v1/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /v1/sessions", s.handleListSessions)
	mux.HandleFunc("GET /v1/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("POST /v1/sessions/{id}/refresh", s.handleRefreshSession)
	mux.HandleFunc("POST /v1/sessions/{id}/attach-volumes", s.handleAttachVolumes)
	mux.HandleFunc("GET /v1/sessions/{id}/ws", s.handleWebSocket)
	mux.HandleFunc("GET /v1/volumes", s.handleListVolumes)
	mux.HandleFunc("DELETE /v1/volumes/{id}", s.handleDeleteVolume)
	mux.HandleFunc("POST /v1/admin/volumes/gc", s.handleVolumeGC)
	return s.middleware(mux)
}

func (s *Server) middleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-ID")
		if rid == "" {
			rid = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", rid)
		// Permissive CORS for the dev frontend; production deployments should
		// front this service with a proper proxy.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type,X-Request-ID")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		ctx := context.WithValue(r.Context(), ridKey{}, rid)
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

type ridKey struct{}

func reqID(ctx context.Context) string {
	if v, ok := ctx.Value(ridKey{}).(string); ok {
		return v
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(ctx context.Context, w http.ResponseWriter, status int, code, msg string, retryable bool) {
	writeJSON(w, status, api.ErrorResponse{
		Code:      code,
		Message:   msg,
		Retryable: retryable,
		RequestID: reqID(ctx),
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	ver, err := s.cfg.Docker.Ping(ctx)
	if err != nil {
		writeErr(r.Context(), w, http.StatusServiceUnavailable, "docker_unreachable", err.Error(), true)
		return
	}
	writeJSON(w, http.StatusOK, api.HealthResponse{Status: "ok", DockerVersion: ver})
}

func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.ImagesResponse{Images: append([]string(nil), AllowedImages...)})
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req api.CreateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(r.Context(), w, http.StatusBadRequest, "invalid_body", err.Error(), false)
		return
	}
	s.createSession(w, r, req, "")
}

// createSession is the shared body of POST /v1/sessions and the
// attach-volumes flow. parentID, when non-empty, scopes attach permission
// to volumes that belong to that session.
func (s *Server) createSession(w http.ResponseWriter, r *http.Request, req api.CreateSessionRequest, parentID string) {
	if !slices.Contains(AllowedImages, req.Image) {
		writeErr(r.Context(), w, http.StatusBadRequest, "image_not_allowed",
			fmt.Sprintf("image %q not in allowlist", req.Image), false)
		return
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	if ttl == 0 {
		ttl = s.cfg.DefaultTTL
	}
	if ttl > s.cfg.MaxTTL {
		ttl = s.cfg.MaxTTL
	}
	now := s.cfg.Now()
	id := uuid.NewString()

	// Resolve the volume mounts: newly-requested volumes are created
	// before the container starts; attached volumes are looked up in the
	// volume table and their MountPath is honored.
	mounts, createdVolumes, err := s.resolveVolumeMounts(r.Context(), id, parentID, req)
	if err != nil {
		writeErr(r.Context(), w, http.StatusBadRequest, "volume_resolution_failed", err.Error(), false)
		return
	}

	spec := docker.ContainerSpec{
		ID:           id,
		Image:        req.Image,
		Owner:        s.cfg.Owner,
		Env:          req.Env,
		WorkingDir:   req.WorkingDir,
		Command:      req.Command,
		LeaseUntil:   now.Add(ttl),
		CreatedAt:    now,
		CreatedBy:    s.cfg.PID,
		VolumeMounts: mounts,
	}
	cid, err := s.cfg.Docker.CreateContainer(r.Context(), spec)
	if err != nil {
		// If we already provisioned volumes for this session, leave them
		// in the table — the operator can clean them up via the volumes
		// API; deleting them on partial failure would surprise callers
		// that explicitly opted into persistent storage.
		writeErr(r.Context(), w, http.StatusInternalServerError, "create_failed", err.Error(), true)
		return
	}
	if err := s.cfg.Docker.StartContainer(r.Context(), cid); err != nil {
		_ = s.cfg.Docker.RemoveContainer(r.Context(), cid, true)
		writeErr(r.Context(), w, http.StatusInternalServerError, "start_failed", err.Error(), true)
		return
	}
	sess := s.cfg.Sessions.Add(&session.Session{
		ID:          id,
		ContainerID: cid,
		Image:       req.Image,
		Owner:       s.cfg.Owner,
		WorkingDir:  spec.WorkingDir,
		CreatedAt:   now,
		ExpiresAt:   spec.LeaseUntil,
	})
	if s.cfg.Logger != nil && len(createdVolumes) > 0 {
		s.cfg.Logger.Info("api: session with volumes",
			"session", sess.ID, "volumes_created", len(createdVolumes))
	}
	writeJSON(w, http.StatusCreated, api.CreateSessionResponse{
		ID:          sess.ID,
		ContainerID: sess.ContainerID,
		WSURL:       fmt.Sprintf("/v1/sessions/%s/ws", sess.ID),
		ExpiresAt:   sess.ExpiresAt,
		Image:       sess.Image,
	})
}

// resolveVolumeMounts creates new volumes from req.Volumes and looks up
// existing volume IDs from req.AttachVolumeIDs. Attach is rejected if
// the volume manager is not configured or if an attached volume doesn't
// belong to the parent session (when parentID is set).
func (s *Server) resolveVolumeMounts(ctx context.Context, sessionID, parentID string, req api.CreateSessionRequest) ([]docker.VolumeMount, []api.Volume, error) {
	if len(req.Volumes) == 0 && len(req.AttachVolumeIDs) == 0 {
		return nil, nil, nil
	}
	if s.cfg.Volumes == nil {
		return nil, nil, errors.New("volume manager not configured")
	}
	mounts := make([]docker.VolumeMount, 0, len(req.Volumes)+len(req.AttachVolumeIDs))
	created := make([]api.Volume, 0, len(req.Volumes))
	for _, vs := range req.Volumes {
		v, err := s.cfg.Volumes.CreateForSession(ctx, sessionID, vs)
		if err != nil {
			return nil, nil, fmt.Errorf("create volume %q: %w", vs.Name, err)
		}
		created = append(created, v)
		mounts = append(mounts, docker.VolumeMount{Source: v.DockerVolumeID, Target: v.MountPath})
	}
	for _, vid := range req.AttachVolumeIDs {
		v, err := s.cfg.Volumes.Get(vid)
		if err != nil {
			return nil, nil, fmt.Errorf("attach volume %q: %w", vid, err)
		}
		if parentID != "" && v.SessionID != parentID {
			return nil, nil, fmt.Errorf("volume %q does not belong to session %q", vid, parentID)
		}
		mounts = append(mounts, docker.VolumeMount{Source: v.DockerVolumeID, Target: v.MountPath})
	}
	return mounts, created, nil
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions := s.cfg.Sessions.List()
	out := make([]api.Session, 0, len(sessions))
	for _, sess := range sessions {
		state := "unknown"
		if info, err := s.cfg.Docker.InspectContainer(r.Context(), sess.ContainerID); err == nil {
			state = info.State
		}
		out = append(out, api.Session{
			ID:             sess.ID,
			Image:          sess.Image,
			ContainerID:    sess.ContainerID,
			ContainerState: state,
			CreatedAt:      sess.CreatedAt,
			ExpiresAt:      sess.ExpiresAt,
			WorkingDir:     sess.WorkingDir,
		})
	}
	writeJSON(w, http.StatusOK, api.ListSessionsResponse{Sessions: out})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.cfg.Sessions.Get(id)
	if err != nil {
		writeErr(r.Context(), w, http.StatusNotFound, "not_found", err.Error(), false)
		return
	}
	state := "unknown"
	if info, ierr := s.cfg.Docker.InspectContainer(r.Context(), sess.ContainerID); ierr == nil {
		state = info.State
	}
	writeJSON(w, http.StatusOK, api.Session{
		ID:             sess.ID,
		Image:          sess.Image,
		ContainerID:    sess.ContainerID,
		ContainerState: state,
		CreatedAt:      sess.CreatedAt,
		ExpiresAt:      sess.ExpiresAt,
		WorkingDir:     sess.WorkingDir,
	})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.cfg.Sessions.Get(id)
	if err != nil {
		writeErr(r.Context(), w, http.StatusNotFound, "not_found", err.Error(), false)
		return
	}
	_ = s.cfg.Docker.StopContainer(r.Context(), sess.ContainerID, 5*time.Second)
	_ = s.cfg.Docker.RemoveContainer(r.Context(), sess.ContainerID, true)
	s.cfg.Sessions.Remove(id)
	// Volumes outlive the session — flag them as released so the
	// retention timer starts. Operators can DELETE individual volumes
	// or wait for the GC sweep.
	if s.cfg.Volumes != nil {
		s.cfg.Volumes.MarkReleased(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAttachVolumes creates a NEW session that mounts the listed
// existing volumes by id. The path's {id} is the parent session that
// originally owned the volumes; the parent does not need to be alive
// because the volume manager carries the durable parent reference. The
// request body may override the image and TTL; otherwise the manager
// falls back to the volume's owner-recorded image (or returns an error
// if the session table no longer carries it).
func (s *Server) handleAttachVolumes(w http.ResponseWriter, r *http.Request) {
	parentID := r.PathValue("id")
	var req api.AttachVolumesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(r.Context(), w, http.StatusBadRequest, "invalid_body", err.Error(), false)
		return
	}
	if len(req.VolumeIDs) == 0 {
		writeErr(r.Context(), w, http.StatusBadRequest, "no_volumes", "volume_ids must be non-empty", false)
		return
	}
	create := api.CreateSessionRequest{
		Image:           req.Image,
		TTLSeconds:      req.TTLSeconds,
		AttachVolumeIDs: req.VolumeIDs,
	}
	if create.Image == "" {
		// Fall back to a still-live parent's image; if the parent is
		// already reaped the caller must supply Image explicitly.
		if parent, err := s.cfg.Sessions.Get(parentID); err == nil {
			create.Image = parent.Image
		}
	}
	if create.Image == "" {
		writeErr(r.Context(), w, http.StatusBadRequest, "image_required",
			"image must be supplied when the parent session has been reaped", false)
		return
	}
	s.createSession(w, r, create, parentID)
}

func (s *Server) handleListVolumes(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Volumes == nil {
		writeJSON(w, http.StatusOK, api.ListVolumesResponse{Volumes: []api.Volume{}})
		return
	}
	sid := r.URL.Query().Get("session_id")
	out := s.cfg.Volumes.List(sid)
	writeJSON(w, http.StatusOK, api.ListVolumesResponse{Volumes: out})
}

func (s *Server) handleDeleteVolume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.cfg.Volumes == nil {
		writeErr(r.Context(), w, http.StatusServiceUnavailable, "volumes_disabled",
			"volume manager not configured", false)
		return
	}
	if err := s.cfg.Volumes.Delete(r.Context(), id); err != nil {
		if errors.Is(err, volume.ErrNotFound) {
			writeErr(r.Context(), w, http.StatusNotFound, "not_found", err.Error(), false)
			return
		}
		writeErr(r.Context(), w, http.StatusInternalServerError, "delete_failed", err.Error(), true)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleVolumeGC(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Volumes == nil {
		writeJSON(w, http.StatusOK, api.VolumeGCResponse{Removed: []string{}})
		return
	}
	removed, errs := s.cfg.Volumes.GC(r.Context())
	resp := api.VolumeGCResponse{Removed: removed}
	for _, e := range errs {
		resp.Errors = append(resp.Errors, e.Error())
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleRefreshSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.cfg.Sessions.Refresh(id, s.cfg.DefaultTTL)
	if err != nil {
		if errors.Is(err, session.ErrNotFound) {
			writeErr(r.Context(), w, http.StatusNotFound, "not_found", err.Error(), false)
			return
		}
		writeErr(r.Context(), w, http.StatusInternalServerError, "refresh_failed", err.Error(), true)
		return
	}
	writeJSON(w, http.StatusOK, api.RefreshResponse{ID: sess.ID, ExpiresAt: sess.ExpiresAt})
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.cfg.Sessions.Get(id)
	if err != nil {
		writeErr(r.Context(), w, http.StatusNotFound, "not_found", err.Error(), false)
		return
	}
	origins := append([]string{}, s.cfg.WSAllowOrigin...)
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns:  append(origins, "localhost:*", "127.0.0.1:*"),
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		s.cfg.Logger.Warn("ws: accept", "err", err)
		return
	}
	relay := &ws.Relay{
		Conn:       c,
		Session:    sess,
		Manager:    s.cfg.Sessions,
		Docker:     s.cfg.Docker,
		TTL:        s.cfg.DefaultTTL,
		Logger:     s.cfg.Logger,
		DroppedOut: s.cfg.DroppedOut,
	}
	if err := relay.Run(r.Context()); err != nil {
		s.cfg.Logger.Warn("ws: relay", "err", err)
	}
}
