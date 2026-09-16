#!/usr/bin/env bash
set -euo pipefail

# Collect a PGO profile from the stable worker's own cycle.
#
# Usage:  ./scripts/collect-pgo-profile.sh [duration_seconds]
#   Default duration: 60 seconds.
#
# The worker runs one cycle immediately at startup (stable.Checker.Run), so the
# profile window opens on the process's real workload: fetch, parse, resolve,
# the IP-stage filters, then the probes. A whole cycle takes minutes (the
# latency probe owns most of it), so the default 60s window prices the fetch and
# IP-stage half; pass a longer duration to weight the probe half instead.
#
# Prerequisites: the server must be startable on :8080 and pprof on :6060.
#   - config.yaml must be present (default location), with sources configured:
#     an empty source list is the deliberate disable and profiles nothing
#   - Geofeed sources and the configured subscriptions should be reachable

PROFILE_DIR="$(dirname "$0")/.."
DURATION="${1:-60}"
PPROF_PORT=":6060"
SERVER_PORT=":8080"

echo "=== Building server binary (without PGO) ==="
cd "$PROFILE_DIR"

# Temporarily remove default.pgo so the build is clean (unoptimized profile)
HAD_PGO=false
if [ -f default.pgo ]; then
  HAD_PGO=true
  mv default.pgo default.pgo.bak
fi

# Build without PGO and with pprof tag to collect a fresh profile
nix-shell --run "go build -tags pprof -pgo=off -o /tmp/sub-preprocessor-server ."
echo "  Binary: /tmp/sub-preprocessor-server"

# Restore default.pgo if it existed (so subsequent builds use it)
if [ "$HAD_PGO" = true ]; then
  mv default.pgo.bak default.pgo
fi

# Cleanup on exit
cleanup() {
  echo "=== Cleaning up ==="
  if [ -n "${SERVER_PID:-}" ]; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -f /tmp/sub-preprocessor-server /tmp/profile.pprof
}

trap cleanup EXIT

echo "=== Starting server with pprof on $PPROF_PORT ==="
PPROF_ADDR="$PPROF_PORT" /tmp/sub-preprocessor-server &
SERVER_PID=$!

# Wait for server to be ready
echo -n "  Waiting for server..."
for i in $(seq 1 30); do
  if curl -sf "http://127.0.0.1${SERVER_PORT}/healthz" > /dev/null 2>&1; then
    echo " ready (${i}s)"
    break
  fi
  if [ "$i" -eq 30 ]; then
    echo " FAILED"
    exit 1
  fi
  sleep 1
done

# Allow a moment for geofeed loading
sleep 2

echo "=== Profiling the worker's startup cycle for ${DURATION}s ==="

# Collect the CPU profile from pprof
GOOGLE_PROFILE=/tmp/profile.pprof \
  nix-shell --run "go tool pprof -proto -output=/tmp/profile.pprof http://127.0.0.1${PPROF_PORT}/debug/pprof/profile?seconds=${DURATION}"

echo "=== Profile collected ($(wc -c < /tmp/profile.pprof) bytes) ==="

# Which half of the cycle the window covered. Both are legitimate profiles; the
# line only records which one this run priced.
if curl -sfI "http://127.0.0.1${SERVER_PORT}/stable.txt" > /dev/null 2>&1; then
  echo "  /stable.txt answered 200: the window covered a whole cycle, probes included"
else
  echo "  /stable.txt still 503: the window covered the fetch and IP-stage half only"
fi

kill "$SERVER_PID" 2>/dev/null || true
wait "$SERVER_PID" 2>/dev/null || true

# Copy as default.pgo
cp /tmp/profile.pprof default.pgo
echo "  Copied to: default.pgo"

# Quick verification: build with PGO
echo "=== Verifying build with new default.pgo ==="
nix-shell --run "go build -pgo=auto ./..."
echo "  Build OK"

# Show profile overview
echo "=== Profile overview ==="
nix-shell --run "go tool pprof -top -nodecount=15 default.pgo 2>&1 | head -30"

echo ""
echo "Done. New PGO profile is at: default.pgo ($(wc -c < default.pgo) bytes)"
echo "Rebuild with 'nix-shell --run \"make\"' to use it."
