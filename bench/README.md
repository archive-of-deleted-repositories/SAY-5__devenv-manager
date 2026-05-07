# bench/

Two harnesses live here:

1. **Chaos test** (`chaos.sh`) — exercises the load-bearing claim that a
   service crash mid-session leaves zero orphaned containers once the
   service comes back up.
2. **Concurrent provisioning bench** (`concurrent.go`) — spawns N
   sessions in parallel and reports provisioning + first-byte latency
   percentiles, plus the manager's cumulative CPU and RSS during the
   run. The companion `regress/` tool gates a fresh result against
   `bench/baseline.json`.

## How to run

```sh
make chaos
# or with custom knobs:
CHAOS_SESSIONS=5 CHAOS_TTL=15 CHAOS_REAP_INTERVAL=5s bash bench/chaos.sh
```

Requires a local Docker daemon. CI runs a 2-session smoke variant via the
`chaos-smoke` job in `.github/workflows/ci.yml`.

## What it checks

1. Provision `CHAOS_SESSIONS` Alpine sessions (default 5).
2. SIGKILL the server process — daemon-side containers survive.
3. Restart the server. The in-process reaper hydrates from the
   `com.devenv.*` labels on the containers and reaps every record whose
   `com.devenv.lease_until` has lapsed.
4. After at most `CHAOS_MAX_WAIT` seconds (default 60), assert
   `docker ps -a --filter label=com.devenv.id` returns nothing.

The result is written to `bench/chaos-result.json` for inspection.

## Why TTL=15 in the script

So the lease lapses while the server is dead; on restart the reaper sees
`lease_until < now` and reaps via the TTL path rather than the orphan-PID
path. Both code paths are exercised by `internal/reaper/reaper_test.go`.

## Concurrent bench

```sh
make bench               # 100-session full run
BENCH_N=20 make bench    # smaller
make bench-regress       # 10-session smoke gated against bench/baseline.json
```

`run-bench.sh` builds a fresh server binary, starts it on
`127.0.0.1:18080`, runs `bench/concurrent.go` against it, optionally
runs `bench/regress`, then SIGINTs the server and cleans up labeled
containers. Result JSONs are written under `bench/results/` (gitignored)
and uploaded as a CI artifact.

### Latency vocabulary

- `provision_*` — `POST /v1/sessions` to a 201 response with the
  session ID populated.
- `first_byte_*` — WebSocket open to the first inbound `output` frame
  the relay forwards. The bench sends `printf bench-ok\n` over the
  socket so the first byte is deterministic.

### Manager resource use

`-manager-pid` lets the bench shell out to `ps` for the server's
cumulative CPU time and RSS (`cputimes`, `rss` columns). Both are
included in the result JSON.
