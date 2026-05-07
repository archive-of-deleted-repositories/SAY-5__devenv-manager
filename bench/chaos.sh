#!/usr/bin/env bash
#
# Chaos test: prove that a hard kill of the server does not leak containers.
#
# Steps:
#   1. Build the server binary.
#   2. Start it; wait for /healthz.
#   3. Provision N alpine sessions through the API.
#   4. SIGKILL the server PID.
#   5. Restart the server; the in-process reaper hydrates from labels and
#      reaps every container whose lease has lapsed within the reap interval.
#   6. After up to MAX_WAIT seconds, count containers carrying com.devenv.id.
#   7. Emit a JSON result and exit non-zero if any orphans remain.
#
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

N="${CHAOS_SESSIONS:-5}"
TTL_SECONDS="${CHAOS_TTL:-15}"
REAP_INTERVAL="${CHAOS_REAP_INTERVAL:-5s}"
MAX_WAIT="${CHAOS_MAX_WAIT:-60}"
ADDR="${CHAOS_ADDR:-127.0.0.1:18080}"
RESULT_FILE="${CHAOS_RESULT_FILE:-bench/chaos-result.json}"

if ! command -v docker >/dev/null 2>&1; then
  echo "chaos: docker not on PATH" >&2
  exit 2
fi
if ! docker info >/dev/null 2>&1; then
  echo "chaos: docker daemon not reachable" >&2
  exit 2
fi

echo "chaos: pre-cleaning any leftover devenv containers"
docker ps -aq --filter label=com.devenv.id | xargs -I{} docker rm -f {} >/dev/null 2>&1 || true

echo "chaos: pre-pulling alpine:3.20 (avoid network jitter during the run)"
docker pull alpine:3.20 >/dev/null

echo "chaos: building server binary"
mkdir -p bin
go build -trimpath -o bin/server ./cmd/server

start_server() {
  ./bin/server -addr="$ADDR" -default-ttl="${TTL_SECONDS}s" -reap-interval="$REAP_INTERVAL" \
    > "bench/chaos-server.log" 2>&1 &
  echo "$!"
}

wait_health() {
  local pid="$1"
  for _ in $(seq 1 50); do
    if curl -fsS --max-time 1 "http://$ADDR/healthz" >/dev/null 2>&1; then
      return 0
    fi
    if ! kill -0 "$pid" 2>/dev/null; then
      echo "chaos: server died during startup; tail log:" >&2
      tail -n 40 bench/chaos-server.log >&2 || true
      return 1
    fi
    sleep 0.2
  done
  echo "chaos: server did not become healthy in time" >&2
  tail -n 40 bench/chaos-server.log >&2 || true
  return 1
}

echo "chaos: starting server (round 1)"
SERVER_PID="$(start_server)"
trap 'kill -9 "$SERVER_PID" 2>/dev/null || true' EXIT
wait_health "$SERVER_PID"

echo "chaos: provisioning $N sessions"
SESSION_IDS=()
for i in $(seq 1 "$N"); do
  body='{"image":"alpine:3.20","ttl_seconds":'"$TTL_SECONDS"'}'
  resp="$(curl -fsS -X POST "http://$ADDR/v1/sessions" -H 'content-type: application/json' -d "$body")"
  sid="$(printf '%s' "$resp" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')"
  SESSION_IDS+=("$sid")
  echo "chaos:  session $i created: $sid"
done

PRE_KILL_COUNT="$(docker ps -aq --filter label=com.devenv.id | wc -l | tr -d ' ')"
if [ "$PRE_KILL_COUNT" -ne "$N" ]; then
  echo "chaos: expected $N labeled containers before kill, got $PRE_KILL_COUNT" >&2
  exit 3
fi
echo "chaos: pre-kill labeled count=$PRE_KILL_COUNT"

echo "chaos: SIGKILLing server pid=$SERVER_PID"
kill -9 "$SERVER_PID"
wait "$SERVER_PID" 2>/dev/null || true

# Verify the containers survive the server crash (they should — that's the
# whole point: the daemon outlives the manager).
SURVIVED="$(docker ps -aq --filter label=com.devenv.id | wc -l | tr -d ' ')"
echo "chaos: post-kill labeled count=$SURVIVED"

echo "chaos: restarting server (round 2 — reaper should claim orphans)"
SERVER_PID="$(start_server)"
trap 'kill -9 "$SERVER_PID" 2>/dev/null || true' EXIT
wait_health "$SERVER_PID"

echo "chaos: waiting up to ${MAX_WAIT}s for orphan count to reach 0"
START_TS="$(date +%s)"
ORPHANS_REMAINING=-1
ELAPSED=0
while [ "$ELAPSED" -lt "$MAX_WAIT" ]; do
  COUNT="$(docker ps -aq --filter label=com.devenv.id | wc -l | tr -d ' ')"
  if [ "$COUNT" -eq 0 ]; then
    ORPHANS_REMAINING=0
    break
  fi
  sleep 1
  ELAPSED="$(( $(date +%s) - START_TS ))"
done

if [ "$ORPHANS_REMAINING" -ne 0 ]; then
  ORPHANS_REMAINING="$(docker ps -aq --filter label=com.devenv.id | wc -l | tr -d ' ')"
fi

# Capture details for the artifact.
DOCKER_VERSION="$(docker version --format '{{.Server.Version}}')"
TIMESTAMP="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
RESULT_DIR="$(dirname "$RESULT_FILE")"
mkdir -p "$RESULT_DIR"

cat > "$RESULT_FILE" <<JSON
{
  "timestamp": "$TIMESTAMP",
  "sessions_provisioned": $N,
  "ttl_seconds": $TTL_SECONDS,
  "reap_interval": "$REAP_INTERVAL",
  "max_wait_seconds": $MAX_WAIT,
  "wait_until_zero_seconds": $ELAPSED,
  "pre_kill_count": $PRE_KILL_COUNT,
  "post_kill_count": $SURVIVED,
  "orphans_remaining": $ORPHANS_REMAINING,
  "docker_version": "$DOCKER_VERSION"
}
JSON

echo "chaos: result -> $RESULT_FILE"
cat "$RESULT_FILE"

if [ "$ORPHANS_REMAINING" -ne 0 ]; then
  echo "chaos: FAILED — $ORPHANS_REMAINING orphans remained" >&2
  exit 1
fi

echo "chaos: PASSED"
