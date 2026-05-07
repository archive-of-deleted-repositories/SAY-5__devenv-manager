# devenv-manager

A small Go service that hands out Docker-backed dev environments on
demand. Each session is a fresh container with a PTY shell, streamed
to a React frontend over WebSockets. Containers carry a TTL; idle
sessions and orphans from a crashed manager are reaped automatically.

The point of the project is the cleanup story, not the convenience: a
dev-environment manager that leaks containers is worse than no manager
at all. This one is built around three failure modes (covered below)
and is verified end-to-end by a chaos test that hard-kills the server
mid-session.

## What this studies

| Failure mode                             | Defense                                                                                  |
| ---------------------------------------- | ---------------------------------------------------------------------------------------- |
| Service crash mid-session                | Containers labeled with the creator PID; on restart, dead-PID containers are orphan-reaped |
| WebSocket disconnect, container survives | TTL refreshed by WS pings; reaper sweeps lease-expired containers every 30s              |
| Container exits, handlers leak           | The relay subscribes to Docker `die` events and tears down its goroutines on receipt    |

## Module layout

| Path                  | Responsibility                                                            |
| --------------------- | ------------------------------------------------------------------------- |
| `cmd/server`          | HTTP + WebSocket server entry point                                       |
| `cmd/reaper`          | Standalone one-shot reaper for cron use                                   |
| `internal/api`        | REST handlers, request-id middleware, allowlist gate                      |
| `internal/docker`     | Docker SDK wrapper + label conventions; defines a mockable `Client` iface |
| `internal/session`    | In-memory session table; hydrates from labels on startup                  |
| `internal/volume`     | Named-volume manager: per-session creation, retention, GC                 |
| `internal/imagebuild` | Build manager + template registry; allowlisted Dockerfiles only           |
| `infra/templates/`    | Committed Dockerfile templates (node22-typescript, python312-poetry, rust-stable) |
| `internal/ws`         | Bidirectional PTY relay over WebSockets, with backpressure handling       |
| `internal/reaper`     | Decision matrix + the loop that stops + removes containers                |
| `pkg/api`             | Public request/response types (importable by client codebases)            |
| `frontend/`           | React + Vite + xterm.js client                                            |
| `bench/chaos.sh`      | Crash the server mid-session; assert zero orphans on restart              |
| `bench/concurrent.go` | N-session concurrent provision bench (P50/P95/P99 + manager CPU/RSS)      |
| `bench/regress`       | Compare a bench run against `bench/baseline.json` and gate CI             |
| `infra/docker-compose.yml` | Local dev stack (manager + host docker.sock)                         |

## Quickstart

```sh
# Backend
docker compose -f infra/docker-compose.yml up --build -d
# OR for hot-reload:
make dev

# Frontend
make frontend-dev
# Vite serves on http://localhost:5173 and proxies /v1/* to the backend.
```

The default base images allowed for sessions are listed at
`GET /v1/images`:

- `alpine:3.20`
- `ubuntu:24.04`
- `node:22-alpine`
- `python:3.12-slim`
- `golang:1.22-alpine`

## Architecture (rough)

```
  Browser                Manager                   Docker daemon
  ┌──────┐  WebSocket   ┌────────────┐    SDK     ┌───────────────┐
  │xterm │ ◄─JSON─────► │ ws relay   │ ─exec───► │ container PTY  │
  │      │              │ ws/relay   │ ◄─events──│ (alpine, ...)  │
  └──────┘              ├────────────┤            └───────────────┘
                        │ session    │ list/create/remove
                        │ manager    │ ◄──────────────────────────
                        ├────────────┤
                        │ reaper     │ ──ListLabeledContainers──►
                        │ loop 30s   │     evaluate ttl + pid
                        └────────────┘
```

Identity is durable in the daemon itself: every container carries
`com.devenv.id`, `com.devenv.lease_until`, and `com.devenv.created_by_pid`.
Restart the manager and it rebuilds its session table from labels.
See `ARCHITECTURE.md` for the full reasoning.

## API surface (v1)

| Method | Path                                       | Notes                                           |
| ------ | ------------------------------------------ | ----------------------------------------------- |
| GET    | `/healthz`                                 | reports Docker daemon reachability              |
| GET    | `/v1/images`                               | the allowlist                                   |
| POST   | `/v1/sessions`                             | creates and starts; body: image, ttl_seconds, volumes |
| GET    | `/v1/sessions`                             | lists sessions known to the manager             |
| GET    | `/v1/sessions/{id}`                        | session detail                                  |
| DELETE | `/v1/sessions/{id}`                        | stop + remove (volumes survive)                 |
| POST   | `/v1/sessions/{id}/refresh`                | extend TTL                                      |
| POST   | `/v1/sessions/{id}/attach-volumes`         | new session reattaching prior volumes by id     |
| GET    | `/v1/sessions/{id}/ws`                     | WebSocket upgrade for the PTY relay             |
| GET    | `/v1/volumes?session_id=`                  | list named volumes                              |
| DELETE | `/v1/volumes/{id}`                         | delete a named volume                           |
| POST   | `/v1/admin/volumes/gc`                     | sweep volumes past their retention window       |
| POST   | `/v1/images/build`                         | build a custom image from an allowlisted template |
| GET    | `/v1/images/{id}`                          | image build status                              |
| GET    | `/v1/images/{id}/log`                      | accumulated build log (text/plain)              |
| GET    | `/metrics`                                 | Prometheus exposition                           |

WebSocket frames are JSON-typed: `input`, `output`, `resize`, `ping`,
`pong`, `close`. See `pkg/api/types.go` for the canonical definitions
and `frontend/src/lib/ws.ts` for the typed client.

## Tests

| Suite                       | Where                                            |
| --------------------------- | ------------------------------------------------ |
| Go unit tests               | `internal/{api,docker,reaper,session,ws}/*_test.go` |
| Go integration tests        | `internal/docker/integration_test.go` (build tag) |
| Frontend unit tests (vitest)| `frontend/src/tests/*.test.ts`                   |
| Chaos test                  | `bench/chaos.sh`                                 |

Local run:

```sh
make test                 # Go + frontend unit
RUN_INTEGRATION=1 make test-integration   # Go + real Docker
make chaos                # full chaos test (5 sessions)
```

CI runs all of the above plus:

- `chaos-smoke` (2 sessions)
- `bench-smoke` (10 sessions, gated against `bench/baseline.json` at 3x tolerance)

## Bench

`bench/concurrent.go` spawns N parallel sessions against a running
manager, recording provisioning latency (POST → 201 with session ID) and
first-byte latency (WS open → first inbound output frame) at P50/P95/P99,
plus the manager's cumulative CPU time and RSS via `ps`. Output JSON is
written under `bench/results/<unix>.json` (gitignored).

```sh
make bench               # full 100-session run
BENCH_N=20 make bench    # smaller run
make bench-regress       # 10-session smoke gated against bench/baseline.json
```

The regress gate fails if any of `provision_p50`, `provision_p95`,
`provision_p99`, or `first_byte_p95` exceeds the baseline by more than
the configured tolerance (default 2.0x; CI uses 3.0x for the smoke).
When the baseline is missing the gate prints a message and exits 0; that
escape hatch is what lets a fresh checkout bootstrap a baseline locally.

### Chaos test result

A real local run on Docker 29.2.1 (the result is committed at
`bench/chaos-result.json`):

```json
{
  "timestamp": "2026-05-07T07:25:20Z",
  "sessions_provisioned": 5,
  "ttl_seconds": 15,
  "reap_interval": "5s",
  "max_wait_seconds": 60,
  "wait_until_zero_seconds": 26,
  "pre_kill_count": 5,
  "post_kill_count": 5,
  "orphans_remaining": 0,
  "docker_version": "29.2.1"
}
```

5 sessions provisioned, server SIGKILLed mid-flight, the daemon-side
containers survived the crash (`post_kill_count = 5`), and on restart
the reaper claimed every one of them within 26 seconds.
**`orphans_remaining = 0`.**

## Cross-session state

Sessions are ephemeral by default — `/workspace` is a tmpfs and the
rootfs is read-only. To carry filesystem state between sessions, request
a named volume at create time:

```sh
curl -X POST http://localhost:8080/v1/sessions \
  -H 'content-type: application/json' \
  -d '{
    "image": "alpine:3.20",
    "volumes": [{"name": "workspace", "mount_path": "/data"}]
  }'
```

The manager creates a Docker named volume `devenv-{sid}-workspace` and
mounts it at `/data`. When the session is reaped (TTL expiry, orphan
sweep, or DELETE), the volume is **NOT** deleted; it is flagged
`released` and the retention timer (default 24h) starts. Until the
retention window closes, the volume can be re-mounted into a new
session via the attach-volumes route:

```sh
curl -X POST http://localhost:8080/v1/sessions/{old-session-id}/attach-volumes \
  -H 'content-type: application/json' \
  -d '{"volume_ids": ["<volume-id>"], "image": "alpine:3.20"}'
```

The new session inherits the volume mounts at the path recorded when
the volume was created. Files written by the prior session are still
present.

Lifecycle endpoints:

| Method | Path                                | Notes                                         |
| ------ | ----------------------------------- | --------------------------------------------- |
| GET    | `/v1/volumes?session_id=`           | list volumes (filterable by session)          |
| DELETE | `/v1/volumes/{id}`                  | unconditional delete                          |
| POST   | `/v1/admin/volumes/gc`              | sweep volumes past their retention window     |
| POST   | `/v1/sessions/{id}/attach-volumes`  | start a new session attaching listed volumes  |

The volume retention default is set by `-volume-retention=24h` at the
server flag layer; tighten it for short-lived environments, lengthen it
when sessions are routinely reattached after a delay.

## Custom images via templates

The static `AllowedImages` list (alpine, ubuntu, node, python, golang)
is the default. For more specific environments, the build endpoint
produces images from a closed set of committed Dockerfile templates:

| Template            | Base                  | Adds                                  |
| ------------------- | --------------------- | ------------------------------------- |
| `node22-typescript` | `node:22-alpine`      | global pnpm, ts-node, typescript      |
| `python312-poetry`  | `python:3.12-slim`    | poetry (system-wide)                  |
| `rust-stable`       | `rust:1-slim`         | sccache, cargo build essentials       |

Submit a build:

```sh
curl -X POST http://localhost:8080/v1/images/build \
  -H 'content-type: application/json' \
  -d '{"template":"node22-typescript","build_args":{"PNPM_VERSION":"9"}}'
# -> {"id":"...","status":"queued","image_ref":"devenv-builds/node22-typescript:..."}
```

Poll status:

```sh
curl http://localhost:8080/v1/images/<build-id>
# -> {"status":"ready", "image_ref":"...", ...}
```

Once the build is `ready`, the image becomes available in `/v1/images`
for that owner; `POST /v1/sessions` accepts the new tag.

### Security boundary

The build endpoint accepts **template names**, not Dockerfiles. The
allowlist is enforced both by the on-disk registry and by a hard-coded
list (`internal/imagebuild.AllowedTemplates`); arbitrary Dockerfiles
cannot reach the engine even if the disk-backed templates directory is
swapped at runtime. Adding a template requires committing a Dockerfile
under `infra/templates/<name>/Dockerfile` AND adding the name to
`AllowedTemplates`. The duplication is intentional.

Build resource caps:

| Limit          | Value                                                          |
| -------------- | -------------------------------------------------------------- |
| Wall clock     | 5 minutes (server flag `-build-max-wall-clock`)                |
| CPU / memory   | Enforced at the daemon level (1 CPU / 1 GiB target)            |

## What this is *not*

- **Not authenticated.** Single-user assumption. The `owner` label is
  the seam where SSO can slot in later.
- **Not multi-tenant** in any meaningful sense — the resource limits
  are per-container, but there is no per-user quota system.
- **Not Kubernetes.** Docker Engine only.
- ~~Not persistent.~~ Sessions are ephemeral by default but can opt
  into named volumes that survive reaping; see "Cross-session state"
  below. There are still no bind mounts and the rootfs remains
  read-only.
- ~~Not a build farm.~~ The build endpoint accepts a closed set of
  Dockerfile templates (see "Custom images via templates"); arbitrary
  Dockerfiles are still rejected.
- **No collaborative sessions.** One WS per session.
- **No GPU, no privileged containers, no host networking.**
- **No audit log** beyond structured stdout logs.

## License

MIT. See `LICENSE`.
