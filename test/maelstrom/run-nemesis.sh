#!/usr/bin/env bash
# Crash and fault-injection harness. The adapter supervises yasdb on a
# persistent file store, and a chaos loop (YASDB_CHAOS_MS) kills yasdb with
# SIGKILL and restarts it on a cadence while the Maelstrom kafka workload runs.
# Maelstrom's log-consistency checker then verifies that every acked write
# survived the crashes and the recoveries. That is the "ack only after durable"
# contract, tested across hard kills.
#
# Chaos stops QUIESCE seconds before the time limit, so yasdb is up for the last
# seconds of the run and the reader can poll the newest acked records before
# Maelstrom's final check. Without the quiesce window a run can end with yasdb
# down or mid-restart, and the checker reports those records as :unseen. That
# reads like lost data and is not. See README.md.
#
#   MAELSTROM=/path/to/maelstrom test/maelstrom/run-nemesis.sh <sync|notifier>
#   CHAOS_MS=4000 CONCURRENCY=8 RATE=100 TIME=60 ... run-nemesis.sh notifier
set -euo pipefail

# The mode is a required argument, not a default. yasdb treats any -durability
# value other than "notifier" as sync, so a missing or misspelled mode would run
# sync, print a pass, and leave the reader believing the pass came from notifier.
DUR="${1-}"
case "$DUR" in
  sync | notifier) ;;
  *)
    echo "run-nemesis.sh: durability mode must be 'sync' or 'notifier', got '$DUR'." >&2
    echo "  test/maelstrom/run-nemesis.sh notifier" >&2
    echo "  make maelstrom-chaos DUR=notifier" >&2
    exit 2
    ;;
esac
MAELSTROM="${MAELSTROM:-maelstrom}"
TIME="${TIME:-60}"
QUIESCE="${QUIESCE:-12}"
CHAOS_WINDOW=$(( TIME - QUIESCE ))
if (( CHAOS_WINDOW < 1 )); then
  echo "TIME=${TIME}s leaves no room for the ${QUIESCE}s quiesce window. Raise TIME or lower QUIESCE." >&2
  exit 2
fi
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
LIB="$ROOT/lib"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "building yasdb and adapter…"
CGO_ENABLED=1 CGO_LDFLAGS="-L$LIB -Wl,-rpath,$LIB" go build -o "$TMP/yasdb" "$ROOT"
CGO_ENABLED=0 go build -o "$TMP/adapter" "$ROOT/cmd/maelstrom-adapter"

# The store survives every crash and restart within a run. Set YASDB_DATA to a
# path outside $TMP to keep it after the run as well, and read the keys back.
# See the ":unseen" section of README.md.
DATA="${YASDB_DATA:-$TMP/yasdb-data}"
mkdir -p "$DATA"

echo "running the maelstrom kafka workload with chaos crashes every ${CHAOS_MS:-5000}ms for ${CHAOS_WINDOW}s of ${TIME}s (durability=$DUR)…"
# Maelstrom spawns the adapter, which inherits the variables below. The adapter
# starts yasdb itself, then crashes and restarts it on the chaos cadence.
YASDB_BIN="$TMP/yasdb" YASDB_DATA="$DATA" YASDB_ADDR=127.0.0.1:4700 \
YASDB_DUR="$DUR" YASDB_FLUSH="${YASDB_FLUSH:-5ms}" YASDB_CHAOS_MS="${CHAOS_MS:-5000}" \
YASDB_CHAOS_WINDOW_MS="$(( CHAOS_WINDOW * 1000 ))" \
LD_LIBRARY_PATH="$LIB" \
  "$MAELSTROM" test -w kafka --bin "$TMP/adapter" \
    --node-count 1 --concurrency "${CONCURRENCY:-8}" --rate "${RATE:-100}" \
    --time-limit "$TIME"
