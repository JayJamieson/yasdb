#!/usr/bin/env bash
# ramp-catchup — read throughput under a ramping rate of history reads. Each
# request reads a seeded stream from offset=-1 (one bounded scan), stressing
# the SlateDB scan / per-record FFI path (BENCHMARKS.md's read bottleneck).
#
#   PEAK_RATE=3000 SEED_RECORDS=5000 ./benchmark/ramp-catchup.sh
set -euo pipefail
cd "$(dirname "$0")"
source ./lib.sh

: "${PEAK_RATE:=3000}"
: "${RAMP:=4m}"
: "${SEED_RECORDS:=1000}"
: "${MAX_WORKERS:=300}"

# loadshape provisions its own pool AND seeds it (-seed-records) — see
# ramp-append.sh's comment on why lib.sh's provision() isn't called here.
loadshape \
	-admin-url "$ADMIN_URL" -target-url "$BASE_URL" -streams "$STREAMS" \
	-op read -seed-records "$SEED_RECORDS" -max-workers "$MAX_WORKERS" \
	-start-rate 0 \
	-stages "[{\"duration\":\"${RAMP}\",\"target\":${PEAK_RATE}}]"
