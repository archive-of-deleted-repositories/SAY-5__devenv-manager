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
	mux.HandleFunc("GET /v1/sessions/{id}/ws", s.handleWebSocket)
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
	spec := docker.ContainerSpec{
		ID:         id,
		Image:      req.Image,
		Owner:      s.cfg.Owner,
		Env:        req.Env,
		WorkingDir: req.WorkingDir,
		Command:    req.Command,
		LeaseUntil: now.Add(ttl),
		CreatedAt:  now,
		CreatedBy:  s.cfg.PID,
	}
	cid, err := s.cfg.Docker.CreateContainer(r.Context(), spec)
	if err != nil {
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
	writeJSON(w, http.StatusCreated, api.CreateSessionResponse{
		ID:          sess.ID,
		ContainerID: sess.ContainerID,
		WSURL:       fmt.Sprintf("/v1/sessions/%s/ws", sess.ID),
		ExpiresAt:   sess.ExpiresAt,
		Image:       sess.Image,
	})
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
	w.WriteHeader(http.StatusNoContent)
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
