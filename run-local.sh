#!/usr/bin/env bash
# Local conformance runner: build the Go forge, serve it, drive it with the sealed sdk suite.
set -u
cd "$(dirname "$0")"
export PATH="$HOME/.local/bin:$PATH"
KIT=/home/mjasb/makefun-earn/work/github-core-clone-01-kit
PORT="${FORGE_PORT:-8090}"
TOKEN="${MAKEFUN_SHOP_TOKEN:-testtok}"
CGO_ENABLED=0 go build -ldflags="-s -w" -o forge . || { echo "BUILD FAILED"; exit 1; }
MAKEFUN_SHOP_TOKEN="$TOKEN" FORGE_PORT="$PORT" ./forge >/tmp/forge.log 2>&1 &
FPID=$!
trap 'kill $FPID 2>/dev/null' EXIT
for i in $(seq 1 50); do
  if curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
node --experimental-strip-types "$KIT/sdk/run.ts" --url "http://127.0.0.1:$PORT" --token "$TOKEN" "$@"