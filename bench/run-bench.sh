#!/usr/bin/env bash
#
# Runs the concurrent bench against a fresh local manager and, if
# BENCH_GATE=1 is set, gates the result against bench/baseline.json.
#
# Knobs:
#   BENCH_N            number of sessions (default 100)
#   BENCH_CONCURRENCY  in-flight cap (default = BENCH_N)
#   BENCH_TTL          per-session TTL seconds (default 120)
#   BENCH_ADDR         manager listen addr (default 127.0.0.1:18080)
#   BENCH_OUT          result JSON path (default bench/results/<unix>.json)
#   BENCH_GATE         if "1", run regress against bench/baseline.json
#   BENCH_TOLERANCE    regression multiplier (default 2.0)
#
# Exit code: 0 on success, 1 on regression or session failures.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

N="${BENCH_N:-100}"
CONCURRENCY="${BENCH_CONCURRENCY:-$N}"
TTL="${BENCH_TTL:-120}"
ADDR="${BENCH_ADDR:-127.0.0.1:18080}"
OUT="${BENCH_OUT:-bench/results/$(date +%s).json}"
GATE="${BENCH_GATE:-0}"
TOLERANCE="${BENCH_TOLERANCE:-2.0}"

mkdir -p "$(dirname "$OUT")"

if ! command -v docker >/dev/null 2>&1; then
  echo "bench: docker not on PATH" >&2
  exit 2
fi
if ! docker info >/dev/null 2>&1; then
  echo "bench: docker daemon not reachable" >&2
  exit 2
fi

echo "bench: pre-cleaning leftover devenv containers"
docker ps -aq --filter label=com.devenv.id | xargs -I{} docker rm -f {} >/dev/null 2>&1 || true

echo "bench: pre-pulling alpine:3.20"
docker pull alpine:3.20 >/dev/null

echo "bench: building server + bench tools"
mkdir -p bin
go build -trimpath -o bin/server ./cmd/server
go build -trimpath -o bin/bench ./bench/
go build -trimpath -o bin/bench-regress ./bench/regress

LOG_FILE="bench/results/server-$(date +%s).log"
mkdir -p "$(dirname "$LOG_FILE")"
./bin/server -addr="$ADDR" -default-ttl="${TTL}s" -reap-interval=30s \
  > "$LOG_FILE" 2>&1 &
SERVER_PID="$!"
trap 'kill -9 "$SERVER_PID" 2>/dev/null || true; docker ps -aq --filter label=com.devenv.id | xargs -I{} docker rm -f {} >/dev/null 2>&1 || true' EXIT

# Wait for /healthz.
for _ in $(seq 1 50); do
  if curl -fsS --max-time 1 "http://$ADDR/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done
if ! curl -fsS --max-time 1 "http://$ADDR/healthz" >/dev/null 2>&1; then
  echo "bench: server never reported healthy; tail of log:" >&2
  tail -n 40 "$LOG_FILE" >&2 || true
  exit 1
fi
echo "bench: server up at $ADDR (pid $SERVER_PID, log $LOG_FILE)"

set +e
./bin/bench -addr="$ADDR" -n="$N" -concurrency="$CONCURRENCY" -ttl="$TTL" \
  -manager-pid="$SERVER_PID" -out="$OUT"
BENCH_EXIT="$?"
set -e
echo "bench: result -> $OUT (exit=$BENCH_EXIT)"

if [ "$GATE" = "1" ]; then
  ./bin/bench-regress -current="$OUT" -baseline=bench/baseline.json -tolerance="$TOLERANCE"
fi

# Reap manager + leftover containers.
kill -INT "$SERVER_PID" 2>/dev/null || true
wait "$SERVER_PID" 2>/dev/null || true
docker ps -aq --filter label=com.devenv.id | xargs -I{} docker rm -f {} >/dev/null 2>&1 || true

exit "$BENCH_EXIT"
