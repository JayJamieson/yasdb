#!/usr/bin/env bash
# Run a benchmark/ script from the in-region load generator against yasdb
# over 6PN. See ../../benchmark/README.md for what each script does.
#
#   ./deploy/loadtest/run.sh                                    # max-throughput, defaults
#   ./deploy/loadtest/run.sh max-throughput WORKERS=400 STREAMS=32
#   ./deploy/loadtest/run.sh ramp-append PEAK_RATE=2000 RAMP=5m
#   ./deploy/loadtest/run.sh smoke
#
# Env: LOADTEST_APP (default yasdb-loadtest), TARGET (default
# http://yasdb.internal:4437). Trailing KEY=VAL args become env vars for the
# script (see benchmark/lib.sh for what each script reads).
#
# ADMIN_URL (bulk-provision) is derived from TARGET automatically, assuming
# the server's -metrics-addr is :9091 (fly.toml's [metrics] block).
# Override by passing ADMIN_URL=... explicitly if that is not the case.
set -euo pipefail

APP="${LOADTEST_APP:-yasdb-loadtest}"
TARGET="${TARGET:-http://yasdb.internal:4437}"
test="${1:-max-throughput}"; shift || true

admin_default="${TARGET%:*}:9091/__admin/bulk-provision"

echo "==> $test  ->  $TARGET   (from $APP, in-region over 6PN)"

envs="BASE_URL=$TARGET ADMIN_URL=$admin_default"
for kv in "$@"; do
  envs="$envs $kv"
done

fly ssh console -a "$APP" -C "bash -c 'cd /benchmark && env $envs ./${test}.sh'"
