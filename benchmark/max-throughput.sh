#!/usr/bin/env bash
# max-throughput — find the ceiling append rate, not a target rate. This is
# a closed model: WORKERS fire back-to-back as fast as the server answers,
# so the reported throughput is the sustained ceiling at that concurrency.
# Sweep STREAMS to see how it scales. Sweep WORKERS to confirm saturation:
# if raising WORKERS still raises throughput, you are client-limited, not
# server-limited.
#
#   STREAMS=32768 WORKERS=800 DURATION=30s ./benchmark/max-throughput.sh
set -euo pipefail
cd "$(dirname "$0")"
source ./lib.sh

: "${WORKERS:=400}"
: "${DURATION:=30s}"

provision
targets=$(mktemp)
payload=$(mktemp)
trap 'rm -f "$targets" "$payload"' EXIT
gen_targets "$targets"
gen_payload "$payload"

vegeta attack \
	-targets="$targets" -body="$payload" \
	-workers="$WORKERS" -max-workers="$WORKERS" \
	-rate=0 -duration="$DURATION" \
	| vegeta report
