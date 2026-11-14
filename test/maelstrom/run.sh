#!/usr/bin/env bash
# Runs the Jepsen Maelstrom "kafka" log-consistency check against yasdb.
#
# The script builds a yasdb server and the maelstrom-adapter, a pure-Go bridge
# from the Maelstrom kafka RPCs to yasdb HTTP. It starts the server, then lets
# Maelstrom's checker validate the append and read path under concurrency.
# Offsets must be unique and monotonic per key, and no message may be lost,
# duplicated, or reordered.
#
# Requires a JDK and Maelstrom, extracted from
#   https://github.com/jepsen-io/maelstrom/releases
# Point MAELSTROM at its launcher script. It defaults to `maelstrom` on PATH.
#
# Usage:
#   MAELSTROM=/path/to/maelstrom/maelstrom test/maelstrom/run.sh <sync|notifier>
#   CONCURRENCY=20 RATE=500 TIME=30 test/maelstrom/run.sh notifier
set -euo pipefail

# The mode is a required argument, not a default. yasdb treats any -durability
# value other than "notifier" as sync, so a missing or misspelled mode would run
# sync, print a pass, and leave the reader believing the pass came from notifier.
DURABILITY="${1-}"
case "$DURABILITY" in
  sync | notifier) ;;
  *)
    echo "run.sh: durability mode must be 'sync' or 'notifier', got '$DURABILITY'." >&2
    echo "  test/maelstrom/run.sh notifier" >&2
    echo "  make maelstrom DUR=notifier" >&2
    exit 2
    ;;
esac
ADDR="${YASDB_ADDR:-127.0.0.1:4500}"
MAELSTROM="${MAELSTROM:-maelstrom}"
READ="${READ:-catchup}"                 # catchup or long-poll, where long-poll drives the live-read and cache path
LPTIMEOUT="${LPTIMEOUT:-500ms}"         # the server's long-poll timeout, short so a caught-up poll returns 204 without a wait
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
LIB="$ROOT/lib"
TMP="$(mktemp -d)"

srv=""
cleanup() { [ -n "$srv" ] && kill -INT "$srv" 2>/dev/null || true; rm -rf "$TMP"; }
trap cleanup EXIT

echo "building yasdb and adapter…"
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
