#!/usr/bin/env bash
set -euo pipefail

# Collect a PGO profile from realistic HTTP traffic.
#
# Usage:  ./scripts/collect-pgo-profile.sh [duration_seconds]
#   Default duration: 60 seconds.
#
# Prerequisites: the server must be startable on :8080 and pprof on :6060.
#   - config.yaml must be present (default location)
#   - Geofeed sources should be reachable (the server loads them at startup)
#   - A subscription URL that works-ish (default: the one from the Makefile)

PROFILE_DIR="$(dirname "$0")/.."
DURATION="${1:-60}"
PPROF_PORT=":6060"
SERVER_PORT=":8080"
SUBSCRIPTION_URL="https://mifa.world/vless"

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

echo "=== Generating HTTP traffic for ${DURATION}s ==="

# Background: hammer the server with concurrent requests simulating diverse traffic
hammer() {
  local regions=(
    "FI,EE,LV,LT,SE,PL,DE,NL"
    "US,CA"
    "GB,FR,DE,IT,ES"
    "JP,KR,SG"
    "AU,NZ"
    "BR,AR,CL"
  )
  local urls=(
    "$SUBSCRIPTION_URL"
    "https://mifa.world/vless"
    "https://example.com/sub"
  )

  end=$((SECONDS + DURATION + 5))
  while [ "$SECONDS" -lt "$end" ]; do
    region="${regions[$((RANDOM % ${#regions[@]}))]}"
    url="${urls[$((RANDOM % ${#urls[@]}))]}"
    curl -sf \
      "http://127.0.0.1${SERVER_PORT}/?subscription_url=${url}&countries=${region}" \
      -o /dev/null 2>/dev/null || true
  done
}

# Start 8 concurrent hammer workers
for _ in $(seq 1 8); do
  hammer &
done

echo "  Hammer workers started, collecting profile..."

# Collect the CPU profile from pprof
GOOGLE_PROFILE=/tmp/profile.pprof \
  nix-shell --run "go tool pprof -proto -output=/tmp/profile.pprof http://127.0.0.1${PPROF_PORT}/debug/pprof/profile?seconds=${DURATION}"

echo "=== Profile collected ($(wc -c < /tmp/profile.pprof) bytes) ==="

# Stop the server and hammer workers
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
