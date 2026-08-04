#!/usr/bin/env bash
# spike-append — sudden burst of appends, then sustain, then drop. This
# exercises the streamer's group-commit coalescing and durability
# pipelining under a step load. STREAMS=1 concentrates the spike on one
# streamer, the worst case for coalescing.
#
#   PEAK_RATE=4000 STREAMS=1 ./benchmark/spike-append.sh
set -euo pipefail
cd "$(dirname "$0")"
source ./lib.sh

: "${START_RATE:=50}"
: "${PEAK_RATE:=600}"
: "${END_RATE:=30}"
: "${MAX_WORKERS:=400}"

# loadshape provisions its own pool — see ramp-append.sh's comment.
loadshape \
	-admin-url "$ADMIN_URL" -target-url "$BASE_URL" -streams "$STREAMS" \
	-op append -payload-bytes "$PAYLOAD_BYTES" -max-workers "$MAX_WORKERS" \
	-start-rate 0 \
	-stages "[
		{\"duration\":\"10s\",\"target\":${START_RATE}},
		{\"duration\":\"1m\",\"target\":${START_RATE}},
		{\"duration\":\"10s\",\"target\":${PEAK_RATE}},
		{\"duration\":\"3m\",\"target\":${PEAK_RATE}},
		{\"duration\":\"10s\",\"target\":${END_RATE}},
		{\"duration\":\"2m\",\"target\":${END_RATE}},
		{\"duration\":\"10s\",\"target\":0}
	]"
