#!/usr/bin/env bash
# ramp-append — write throughput under a linearly increasing append rate.
#
#   PEAK_RATE=2000 RAMP=5m ./benchmark/ramp-append.sh
set -euo pipefail
cd "$(dirname "$0")"
source ./lib.sh

: "${PEAK_RATE:=600}"
: "${RAMP:=4m}"
: "${MAX_WORKERS:=300}"

# loadshape provisions its own pool (under /loadshape/<ts>/s). Do not call
# lib.sh's provision() here too: it targets a different prefix nothing
# reads.
loadshape \
	-admin-url "$ADMIN_URL" -target-url "$BASE_URL" -streams "$STREAMS" \
	-op append -payload-bytes "$PAYLOAD_BYTES" -max-workers "$MAX_WORKERS" \
	-start-rate 0 \
	-stages "[{\"duration\":\"${RAMP}\",\"target\":${PEAK_RATE}}]"
