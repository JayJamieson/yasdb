#!/usr/bin/env bash
# variable-append — fluctuating append rate (sawtooth between a low and high
# target). Shows how latency tracks a load that never settles.
#
#   LOW_RATE=200 HIGH_RATE=2000 ./benchmark/variable-append.sh
set -euo pipefail
cd "$(dirname "$0")"
source ./lib.sh

: "${LOW_RATE:=100}"
: "${HIGH_RATE:=600}"
: "${MAX_WORKERS:=300}"

# loadshape provisions its own pool — see ramp-append.sh's comment.
loadshape \
	-admin-url "$ADMIN_URL" -target-url "$BASE_URL" -streams "$STREAMS" \
	-op append -payload-bytes "$PAYLOAD_BYTES" -max-workers "$MAX_WORKERS" \
	-start-rate "$LOW_RATE" \
	-stages "[
		{\"duration\":\"1m\",\"target\":${HIGH_RATE}},
		{\"duration\":\"1m\",\"target\":${LOW_RATE}},
		{\"duration\":\"30s\",\"target\":${HIGH_RATE}},
		{\"duration\":\"1m\",\"target\":${LOW_RATE}},
		{\"duration\":\"30s\",\"target\":${HIGH_RATE}},
		{\"duration\":\"1m\",\"target\":${LOW_RATE}}
	]"
