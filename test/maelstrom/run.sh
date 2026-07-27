#!/usr/bin/env bash
# Runs the Jepsen Maelstrom "kafka" log-consistency check against yasdb.
#
# It builds a yasdb server + the maelstrom-adapter (a pure-Go bridge from the
# Maelstrom kafka RPCs to yasdb HTTP), starts the server, and lets Maelstrom's
# checker validate append/read log consistency (unique/monotonic offsets, no
# lost/duplicated/reordered messages) under concurrency.
#
# Prereqs: a JDK, and Maelstrom extracted from
#   https://github.com/jepsen-io/maelstrom/releases
# Point MAELSTROM at its launcher script (defaults to `maelstrom` on PATH).
#
# Usage:
#   MAELSTROM=/path/to/maelstrom/maelstrom test/maelstrom/run.sh [sync|notifier]
#   CONCURRENCY=20 RATE=500 TIME=30 test/maelstrom/run.sh notifier
set -euo pipefail

DURABILITY="${1:-sync}"
ADDR="${YASDB_ADDR:-127.0.0.1:4500}"
MAELSTROM="${MAELSTROM:-maelstrom}"
READ="${READ:-catchup}"                 # catchup | long-poll (long-poll drives the live-read + cache path)
LPTIMEOUT="${LPTIMEOUT:-500ms}"         # server long-poll timeout (short, so caught-up polls return 204 fast)
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
LIB="$ROOT/lib"
TMP="$(mktemp -d)"

srv=""
cleanup() { [ -n "$srv" ] && kill -INT "$srv" 2>/dev/null || true; rm -rf "$TMP"; }
trap cleanup EXIT

echo "building yasdb + adapter…"
CGO_ENABLED=1 CGO_LDFLAGS="-L$LIB -Wl,-rpath,$LIB" go build -o "$TMP/yasdb" "$ROOT"
CGO_ENABLED=0 go build -o "$TMP/adapter" "$ROOT/cmd/maelstrom-adapter"

echo "starting yasdb (durability=$DURABILITY, read=$READ) on $ADDR…"
LD_LIBRARY_PATH="$LIB" "$TMP/yasdb" -addr "$ADDR" -store memory:/// -flush 5ms \
  -durability "$DURABILITY" -notifier-poll 1ms -longpoll-timeout "$LPTIMEOUT" >"$TMP/yasdb.log" 2>&1 &
srv=$!
until curl -sf "http://$ADDR/__health" >/dev/null 2>&1; do sleep 0.3; done

echo "running maelstrom kafka workload…"
YASDB_URL="http://$ADDR" YASDB_MLST_READ="$READ" "$MAELSTROM" test -w kafka --bin "$TMP/adapter" \
  --node-count 1 --concurrency "${CONCURRENCY:-10}" --rate "${RATE:-200}" \
  --time-limit "${TIME:-20}"
