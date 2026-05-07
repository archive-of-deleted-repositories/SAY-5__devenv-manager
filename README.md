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
| `internal/ws`         | Bidirectional PTY relay over WebSockets, with backpressure handling       |
| `internal/reaper`     | Decision matrix + the loop that stops + removes containers                |
| `pkg/api`             | Public request/response types (importable by client codebases)            |
| `frontend/`           | React + Vite + xterm.js client                                            |
| `bench/chaos.sh`      | Crash the server mid-session; assert zero orphans on restart              |
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

| Method | Path                                  | Notes                                           |
| ------ | ------------------------------------- | ----------------------------------------------- |
| GET    | `/healthz`                            | reports Docker daemon reachability              |
| GET    | `/v1/images`                          | the allowlist                                   |
| POST   | `/v1/sessions`                        | creates and starts; body: image, ttl_seconds    |
| GET    | `/v1/sessions`                        | lists sessions known to the manager             |
| GET    | `/v1/sessions/{id}`                   | session detail                                  |
| DELETE | `/v1/sessions/{id}`                   | stop + remove                                   |
| POST   | `/v1/sessions/{id}/refresh`           | extend TTL                                      |
| GET    | `/v1/sessions/{id}/ws`                | WebSocket upgrade for the PTY relay             |
| GET    | `/metrics`                            | Prometheus exposition                           |

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

CI runs all of the above plus a `chaos-smoke` job (2 sessions for speed).

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

## What this is *not*

- **Not authenticated.** Single-user assumption. The `owner` label is
  the seam where SSO can slot in later.
- **Not multi-tenant** in any meaningful sense — the resource limits
  are per-container, but there is no per-user quota system.
- **Not Kubernetes.** Docker Engine only.
- **Not persistent.** Sessions are ephemeral; `/workspace` is a tmpfs.
  No volumes, no bind mounts.
- **Not a build farm.** Image allowlist only — no `docker build`
  exposed, no arbitrary registry pulls.
- **No collaborative sessions.** One WS per session.
- **No GPU, no privileged containers, no host networking.**
- **No audit log** beyond structured stdout logs.

## License

MIT. See `LICENSE`.
