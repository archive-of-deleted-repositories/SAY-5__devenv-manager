package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	dtypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	dockerevents "github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/google/uuid"
)

// Client is the narrow surface of the Docker daemon the service depends on.
// Defining it as an interface keeps the API and reaper packages testable
// without spinning up a real daemon.
type Client interface {
	Ping(ctx context.Context) (string, error)
	CreateContainer(ctx context.Context, spec ContainerSpec) (string, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, timeout time.Duration) error
	RemoveContainer(ctx context.Context, id string, force bool) error
	InspectContainer(ctx context.Context, id string) (ContainerInfo, error)
	ListLabeledContainers(ctx context.Context) ([]ContainerInfo, error)
	ExecAttach(ctx context.Context, containerID string, cmd []string, tty bool) (HijackedConn, error)
	ResizeExec(ctx context.Context, execID string, cols, rows uint16) error
	Events(ctx context.Context, since time.Time) (<-chan EventMessage, <-chan error)
	CreateVolume(ctx context.Context, name string, labels map[string]string) (string, error)
	RemoveVolume(ctx context.Context, name string, force bool) error
	Close() error
}

// ContainerSpec is the manager-level description we translate to Docker.
type ContainerSpec struct {
	ID         string // session UUID — written into LabelID
	Image      string
	Owner      string
	Env        map[string]string
	WorkingDir string
	Command    []string
	LeaseUntil time.Time
	CreatedAt  time.Time
	CreatedBy  int // pid
	// Resource limits
	CPUShares int64
	MemoryMB  int64
	PIDsLimit int64
	// Workspace tmpfs
	WorkspaceTmpfsBytes int64
	// VolumeMounts attaches Docker named volumes at start time. The
	// Source must already exist; the volume manager creates it before
	// the container is started.
	VolumeMounts []VolumeMount
}

// VolumeMount is a Docker named-volume mount applied at container start.
type VolumeMount struct {
	Source string // docker volume name, e.g. devenv-{sid}-{vname}
	Target string // path inside the container
}

// ContainerInfo is the manager-level view of a container, with labels
// already parsed into typed fields.
type ContainerInfo struct {
	ID           string
	Image        string
	State        string
	Created      time.Time
	Labels       map[string]string
	SessionID    string
	Owner        string
	LeaseUntil   time.Time
	CreatedByPID int
	WorkingDir   string
	ExecID       string // populated only after ExecAttach
}

// HijackedConn is a bidirectional byte stream over a Docker exec attach.
type HijackedConn interface {
	io.ReadWriteCloser
	ExecID() string
}

// EventMessage is a minimal projection of Docker daemon events.
type EventMessage struct {
	Action      string
	Type        string
	ContainerID string
	SessionID   string // from labels
	Time        time.Time
}

// realClient is the production implementation that talks to a real daemon.
type realClient struct {
	c *client.Client
}

// New connects to the Docker daemon using the default environment
// (DOCKER_HOST or unix:///var/run/docker.sock).
func New() (Client, error) {
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker: connect: %w", err)
	}
	return &realClient{c: c}, nil
}

func (r *realClient) Close() error { return r.c.Close() }

func (r *realClient) Ping(ctx context.Context) (string, error) {
	p, err := r.c.Ping(ctx)
	if err != nil {
		return "", err
	}
	return p.APIVersion, nil
}

func (r *realClient) CreateContainer(ctx context.Context, spec ContainerSpec) (string, error) {
	if spec.ID == "" {
		spec.ID = uuid.NewString()
	}
	labels := map[string]string{
		LabelID:           spec.ID,
		LabelOwner:        spec.Owner,
		LabelCreatedAt:    strconv.FormatInt(spec.CreatedAt.Unix(), 10),
		LabelLeaseUntil:   strconv.FormatInt(spec.LeaseUntil.Unix(), 10),
		LabelCreatedByPID: strconv.Itoa(spec.CreatedBy),
	}

	envSlice := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		envSlice = append(envSlice, k+"="+v)
	}

	cmd := spec.Command
	if len(cmd) == 0 {
		cmd = []string{"/bin/sh"}
	}
	wd := spec.WorkingDir
	if wd == "" {
		wd = "/workspace"
	}

	// Ensure image is present.
	if err := r.ensureImage(ctx, spec.Image); err != nil {
		return "", err
	}

	cfg := &container.Config{
		Image:        spec.Image,
		Env:          envSlice,
		WorkingDir:   wd,
		Cmd:          cmd,
		Tty:          true,
		OpenStdin:    true,
		StdinOnce:    false,
		Labels:       labels,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	}

	tmpfsBytes := spec.WorkspaceTmpfsBytes
	if tmpfsBytes == 0 {
		tmpfsBytes = 64 * 1024 * 1024 // 64MB default
	}
	memBytes := spec.MemoryMB * 1024 * 1024
	if memBytes == 0 {
		memBytes = 512 * 1024 * 1024
	}
	pidsLimit := spec.PIDsLimit
	if pidsLimit == 0 {
		pidsLimit = 256
	}
	cpuShares := spec.CPUShares
	if cpuShares == 0 {
		cpuShares = 256
	}

	mounts := make([]mount.Mount, 0, len(spec.VolumeMounts))
	for _, vm := range spec.VolumeMounts {
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeVolume,
			Source: vm.Source,
			Target: vm.Target,
		})
	}

	hostCfg := &container.HostConfig{
		AutoRemove:     false, // we manage removal explicitly via the reaper
		ReadonlyRootfs: true,
		NetworkMode:    "bridge",
		Tmpfs: map[string]string{
			"/workspace": fmt.Sprintf("rw,size=%d", tmpfsBytes),
			"/tmp":       fmt.Sprintf("rw,size=%d", tmpfsBytes),
		},
		Mounts: mounts,
		Resources: container.Resources{
			CPUShares: cpuShares,
			Memory:    memBytes,
			PidsLimit: &pidsLimit,
		},
	}

	resp, err := r.c.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "devenv-"+spec.ID[:8])
	if err != nil {
		return "", fmt.Errorf("docker: create: %w", err)
	}
	return resp.ID, nil
}

// ensureImage pulls the image if it isn't already present locally. The
// allowlist is enforced one layer up; here we only translate to the SDK call.
func (r *realClient) ensureImage(ctx context.Context, ref string) error {
	_, _, err := r.c.ImageInspectWithRaw(ctx, ref)
	if err == nil {
		return nil
	}
	rc, perr := r.c.ImagePull(ctx, ref, image.PullOptions{})
	if perr != nil {
		return fmt.Errorf("docker: pull %q: %w", ref, perr)
	}
	defer func() { _ = rc.Close() }()
	if _, copyErr := io.Copy(io.Discard, rc); copyErr != nil {
		return fmt.Errorf("docker: drain pull stream: %w", copyErr)
	}
	return nil
}

func (r *realClient) StartContainer(ctx context.Context, id string) error {
	return r.c.ContainerStart(ctx, id, container.StartOptions{})
}

func (r *realClient) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	err := r.c.ContainerStop(ctx, id, container.StopOptions{Timeout: &secs})
	if err != nil && !client.IsErrNotFound(err) {
		return err
	}
	return nil
}

func (r *realClient) RemoveContainer(ctx context.Context, id string, force bool) error {
	// RemoveVolumes is intentionally false: the volume manager owns the
	// lifecycle of named volumes (separate retention from the container
	// itself). Removing them here would break the cross-session
	// persistence guarantee.
	err := r.c.ContainerRemove(ctx, id, container.RemoveOptions{Force: force, RemoveVolumes: false})
	if err != nil && !client.IsErrNotFound(err) {
		return err
	}
	return nil
}

func (r *realClient) InspectContainer(ctx context.Context, id string) (ContainerInfo, error) {
	j, err := r.c.ContainerInspect(ctx, id)
	if err != nil {
		return ContainerInfo{}, err
	}
	created, _ := time.Parse(time.RFC3339Nano, j.Created)
	info := ContainerInfo{
		ID:      j.ID,
		Image:   j.Config.Image,
		State:   j.State.Status,
		Created: created,
		Labels:  j.Config.Labels,
	}
	parseLabels(&info)
	return info, nil
}

func (r *realClient) ListLabeledContainers(ctx context.Context) ([]ContainerInfo, error) {
	f := filters.NewArgs()
	f.Add("label", LabelID)
	list, err := r.c.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, err
	}
	out := make([]ContainerInfo, 0, len(list))
	for _, c := range list {
		info := ContainerInfo{
			ID:      c.ID,
			Image:   c.Image,
			State:   c.State,
			Created: time.Unix(c.Created, 0),
			Labels:  c.Labels,
		}
		parseLabels(&info)
		out = append(out, info)
	}
	return out, nil
}

func parseLabels(info *ContainerInfo) {
	info.SessionID = info.Labels[LabelID]
	info.Owner = info.Labels[LabelOwner]
	if v, ok := info.Labels[LabelLeaseUntil]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			info.LeaseUntil = time.Unix(n, 0)
		}
	}
	if v, ok := info.Labels[LabelCreatedByPID]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			info.CreatedByPID = n
		}
	}
}

// hijackedConnAdapter wraps a docker types.HijackedResponse into our
// HijackedConn interface.
type hijackedConnAdapter struct {
	hr     dtypes.HijackedResponse
	execID string
}

func (h *hijackedConnAdapter) Read(p []byte) (int, error)  { return h.hr.Reader.Read(p) }
func (h *hijackedConnAdapter) Write(p []byte) (int, error) { return h.hr.Conn.Write(p) }
func (h *hijackedConnAdapter) Close() error                { h.hr.Close(); return nil }
func (h *hijackedConnAdapter) ExecID() string              { return h.execID }

func (r *realClient) ExecAttach(ctx context.Context, containerID string, cmd []string, tty bool) (HijackedConn, error) {
	if len(cmd) == 0 {
		cmd = []string{"/bin/sh"}
	}
	exec, err := r.c.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		Tty:          tty,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("docker: exec create: %w", err)
	}
	hr, err := r.c.ContainerExecAttach(ctx, exec.ID, container.ExecStartOptions{Tty: tty})
	if err != nil {
		return nil, fmt.Errorf("docker: exec attach: %w", err)
	}
	return &hijackedConnAdapter{hr: hr, execID: exec.ID}, nil
}

func (r *realClient) ResizeExec(ctx context.Context, execID string, cols, rows uint16) error {
	return r.c.ContainerExecResize(ctx, execID, container.ResizeOptions{
		Width:  uint(cols),
		Height: uint(rows),
	})
}

// CreateVolume creates a Docker named volume with the given name and
// labels. Returns the volume name (Docker uses the name as the
// canonical identifier; the SDK has no separate ID).
func (r *realClient) CreateVolume(ctx context.Context, name string, labels map[string]string) (string, error) {
	v, err := r.c.VolumeCreate(ctx, volume.CreateOptions{
		Name:   name,
		Driver: "local",
		Labels: labels,
	})
	if err != nil {
		return "", fmt.Errorf("docker: volume create %q: %w", name, err)
	}
	return v.Name, nil
}

// RemoveVolume removes a Docker named volume. Idempotent on missing.
func (r *realClient) RemoveVolume(ctx context.Context, name string, force bool) error {
	err := r.c.VolumeRemove(ctx, name, force)
	if err != nil && !client.IsErrNotFound(err) {
		return fmt.Errorf("docker: volume remove %q: %w", name, err)
	}
	return nil
}

func (r *realClient) Events(ctx context.Context, since time.Time) (<-chan EventMessage, <-chan error) {
	out := make(chan EventMessage, 32)
	errs := make(chan error, 1)
	f := filters.NewArgs()
	f.Add("type", "container")
	msgs, errCh := r.c.Events(ctx, dockerevents.ListOptions{Since: strconv.FormatInt(since.Unix(), 10), Filters: f})

	go func() {
		defer close(out)
		defer close(errs)
		for {
			select {
			case <-ctx.Done():
				return
			case m, ok := <-msgs:
				if !ok {
					return
				}
				ev := EventMessage{
					Action:      string(m.Action),
					Type:        string(m.Type),
					ContainerID: m.Actor.ID,
					Time:        time.Unix(0, m.TimeNano),
				}
				if m.Actor.Attributes != nil {
					ev.SessionID = m.Actor.Attributes[LabelID]
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			case e, ok := <-errCh:
				if !ok {
					return
				}
				if e != nil && !errors.Is(e, io.EOF) {
					select {
					case errs <- e:
					default:
					}
				}
				return
			}
		}
	}()
	return out, errs
}
