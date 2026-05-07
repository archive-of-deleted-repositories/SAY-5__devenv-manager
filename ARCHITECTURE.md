# Architecture

This document explains the design decisions that aren't obvious from the
code, and the failure modes the design is built around.

## Identity layer: Docker labels, not a database

Every container the manager creates carries five labels:

| Label                       | Meaning                                       |
| --------------------------- | --------------------------------------------- |
| `com.devenv.id`             | the session UUID (primary identity)           |
| `com.devenv.owner`          | logical owner (auth-ready)                    |
| `com.devenv.created_at`     | unix seconds, set on `CreateContainer`        |
| `com.devenv.lease_until`    | unix seconds, the TTL boundary                |
| `com.devenv.created_by_pid` | PID of the manager process that owned creation|

These labels are the **durable identity layer**. The in-memory session
table in `internal/session/` is a hot cache; on startup,
`Manager.HydrateFromContainers` repopulates it from the daemon by listing
every container that carries `com.devenv.id`. There is no SQLite, no
external state store, no migration story. The Docker daemon already
keeps the labels durable across manager restarts and reboots; piggy-
backing on that store removes a whole class of "the database and the
daemon disagree" bugs.

A secondary benefit: anyone with shell access to the host can audit and
clean up the manager's footprint with one filter:

```sh
docker ps -a --filter label=com.devenv.id
docker ps -aq --filter label=com.devenv.id | xargs docker rm -f
```

## The three failure modes

The whole reaper exists because of three orthogonal ways a session can
desynchronise from the manager. Each is defended in a different layer.

### 1. Service crash mid-session

Symptom: the manager process dies (panic, OOM, container-restart,
SIGKILL during a deploy). Containers it created keep running on the
daemon.

Defense: every container is labeled with `com.devenv.created_by_pid`.
On startup, the new manager:

1. Calls `ListLabeledContainers` to discover every existing devenv container.
2. Hydrates the session table from labels (so the new process can serve
   `GET /v1/sessions` and answer WS reattach attempts).
3. The reaper loop sees, for each container, a PID that is _not_ the
   current PID. It checks `ProcessAlive(pid)`. If that PID is dead, the
   container is reaped immediately as an orphan.

Either path (lease lapse or orphan PID) resolves the same way: stop +
remove. Both code paths are exercised by `internal/reaper/reaper_test.go`
and end-to-end by `bench/chaos.sh`.

### 2. WebSocket disconnect, container survives

Symptom: a user closes their tab; the WS goes away; nobody is sending
pings; the container is still running, billing CPU/memory.

Defense: every WS `ping` calls `Manager.Refresh(id, defaultTTL)`, which
extends the `lease_until` deadline. The reaper runs every 30 seconds and
removes any container whose lease has lapsed. A 60-second default TTL
plus a 30-second reap interval means the worst-case tail latency from
"user disconnects" to "container reaped" is **lease + reap interval =
60s + 30s = 90s**. Tunable via flags.

### 3. Container exits but the WS handler leaks

Symptom: the user types `exit` inside the container shell. The PTY
closes; the WS handler is still listening for input on the dead socket.

Defense: `internal/ws/protocol.go` runs a `watchDie` goroutine alongside
the read/write pumps. It subscribes to Docker `Events()` and watches for
the `die` event matching the relay's container ID. On `die`, the relay
closes the WebSocket with a normal-closure status and the cleanup
defer chain tears down all goroutines.

## Backpressure: output may drop, input never

Between the Docker exec stdout and the WS write loop sits a buffered
channel of capacity `OutputBuffer = 256` frames (about 1MB at typical
4KB chunks). Three reasons output can fall behind: a slow client, a
congested network, a client that blocked on a render. When the channel
is full:

- **Output frames are dropped** and `DroppedOut.Add(1)` is incremented.
  The Prometheus counter `devenv_dropped_output_frames_total` lets
  operators see this in production.
- **Input frames are NEVER dropped.** Stdin must be lossless or you'd
  end up with users typing `rm -rf foo` and the shell receiving
  `rm -rf fo`. The WS read loop synchronously writes input to the exec
  stdin and only proceeds after it returns.

## Resource limit defaults

| Limit                | Default                              | Rationale                                 |
| -------------------- | ------------------------------------ | ----------------------------------------- |
| CPU shares           | 256 (a quarter of one CPU at parity) | curb runaway sessions                     |
| Memory               | 512 MB                               | enough for a `node` shell, not for a JVM  |
| Pids limit           | 256                                  | fork-bomb defense                         |
| Read-only root       | yes                                  | tampering needs explicit `/workspace`     |
| `/workspace` tmpfs   | 64 MB rw                             | scratch space, no host bind               |
| `/tmp` tmpfs         | 64 MB rw                             | apt/pip occasional use                    |
| Privileged           | never                                | full stop                                 |
| Host network         | no                                   | bridge isolation                          |
| AutoRemove           | **off**                              | reaper owns lifecycle, not the daemon     |

`AutoRemove=true` would make the daemon delete a container the moment
its main process exits — convenient, but it also yanks the labels out
from under the reaper before it gets a chance to see them. The reaper
can't reap an orphan it can't see, so we keep the corpse around long
enough to inspect, then remove explicitly.

## Reaper decision matrix

For each container observed via `ListLabeledContainers`:

| `lease_until` | `created_by_pid`           | Decision    |
| ------------- | -------------------------- | ----------- |
| < now         | (any)                      | reap (TTL)  |
| ≥ now         | matches current PID        | keep        |
| ≥ now         | other PID, alive           | keep        |
| ≥ now         | other PID, dead / unknown  | reap (orphan) |

Lease takes precedence over orphan-PID — `Evaluate` returns `ttl` for
the (expired, dead-PID) cell so we never double-count in metrics.

## What's deliberately not here

- **Auth / multi-tenancy.** Single-user assumption. `owner` label is
  the seam where future auth slots in.
- **Persistent volumes.** Sessions are ephemeral. Saving work means
  copying it out via the terminal, not relying on the manager.
- **GPU / privileged containers.** Out of scope; introduces an isolation
  boundary the rest of the design isn't built for.
- **Image build endpoint.** Allowlist only. Letting users push
  arbitrary build contexts becomes a separate security model.
- **Audit log.** Structured logs go to stdout; if you need persistence,
  ship them to your aggregator of choice.
- **Real-time collaboration on a single session.** One WS per session.
- **Kubernetes.** Docker Engine only. The daemon is the durable store
  and the unit of scheduling; the design unwinds if you swap that out.
