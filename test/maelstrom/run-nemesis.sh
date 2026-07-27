#!/usr/bin/env bash
# Crash / fault-injection harness. The adapter supervises yasdb on a persistent
# file store; a chaos loop (YASDB_CHAOS_MS) SIGKILLs and restarts yasdb on a
# cadence while the Maelstrom kafka workload runs. Maelstrom's log-consistency
# checker then verifies every acked write survived the crashes and recovery —
# i.e. the "ack only after durable" contract holds across hard kills.
#
#   MAELSTROM=/path/to/maelstrom test/maelstrom/run-nemesis.sh [sync|notifier]
#   CHAOS_MS=4000 CONCURRENCY=8 RATE=100 TIME=60 ... run-nemesis.sh notifier
set -euo pipefail

DUR="${1:-sync}"
MAELSTROM="${MAELSTROM:-maelstrom}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
LIB="$ROOT/lib"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "building yasdb + adapter…"
CGO_ENABLED=1 CGO_LDFLAGS="-L$LIB -Wl,-rpath,$LIB" go build -o "$TMP/yasdb" "$ROOT"
CGO_ENABLED=0 go build -o "$TMP/adapter" "$ROOT/cmd/maelstrom-adapter"

DATA="$TMP/yasdb-data"          # persistent across the run's crash/restart cycles
mkdir -p "$DATA"

echo "running maelstrom kafka + chaos crashes every ${CHAOS_MS:-5000}ms (durability=$DUR)…"
# The env below is inherited by the adapter Maelstrom spawns; the adapter starts
# yasdb itself and crashes/restarts it on the chaos cadence.
YASDB_BIN="$TMP/yasdb" YASDB_DATA="$DATA" YASDB_ADDR=127.0.0.1:4700 \
YASDB_DUR="$DUR" YASDB_FLUSH="${YASDB_FLUSH:-5ms}" YASDB_CHAOS_MS="${CHAOS_MS:-5000}" \
LD_LIBRARY_PATH="$LIB" \
  "$MAELSTROM" test -w kafka --bin "$TMP/adapter" \
    --node-count 1 --concurrency "${CONCURRENCY:-8}" --rate "${RATE:-100}" \
    --time-limit "${TIME:-60}"
