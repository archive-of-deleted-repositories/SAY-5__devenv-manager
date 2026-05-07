# bench/

The chaos test exercises the load-bearing claim of this project: a service
crash mid-session leaves zero orphaned containers once the service comes
back up.

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
