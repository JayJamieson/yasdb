#!/bin/sh
# Maps environment variables to yasdb flags. Two storage modes:
#   - object store:  set YASDB_STORE=s3://<bucket> (+ AWS_* env for Tigris/S3)
#   - local volume:  leave YASDB_STORE empty; data goes to YASDB_DATA (a mount)
set -eu

ADDR=":${PORT:-4437}"
FLUSH="${YASDB_FLUSH:-10ms}"
DURABILITY="${YASDB_DURABILITY:-notifier}"

# Optional: a Prometheus /metrics endpoint of native SlateDB engine
# metrics, on its own address (see fly.toml's [metrics] block; Fly scrapes
# this over the private network, not the public one -addr serves).
METRICS_ARGS=""
if [ -n "${YASDB_METRICS_ADDR:-}" ]; then
    METRICS_ARGS="-metrics-addr ${YASDB_METRICS_ADDR}"
fi

# Optional: SlateDB compactor concurrency knobs — see BENCHMARKS.md's
# "Compaction stall under load" section. Tuning these on a shared local box
# was inconclusive, since everything contends for the same cores. Worth
# sweeping again against a real deployment's own load test — see
# docs/DEPLOY.md's "Reading metrics from a load test" section for how to
# pull the numbers back via the Prometheus HTTP API, instead of clicking
# through Grafana by hand.
COMPACTOR_ARGS=""
if [ -n "${YASDB_COMPACTOR_MAX_JOBS:-}" ]; then
    COMPACTOR_ARGS="$COMPACTOR_ARGS -compactor-max-jobs ${YASDB_COMPACTOR_MAX_JOBS}"
fi
if [ -n "${YASDB_COMPACTOR_MAX_SUBCOMPACTIONS:-}" ]; then
    COMPACTOR_ARGS="$COMPACTOR_ARGS -compactor-max-subcompactions ${YASDB_COMPACTOR_MAX_SUBCOMPACTIONS}"
fi
if [ -n "${YASDB_L0_FLUSH_PARALLELISM:-}" ]; then
    COMPACTOR_ARGS="$COMPACTOR_ARGS -l0-flush-parallelism ${YASDB_L0_FLUSH_PARALLELISM}"
fi

# Optional: shard count for the experimental pwal:// backend (see
# internal/ds/pwalstore.go); ignored for every other -store scheme.
PWAL_ARGS=""
if [ -n "${YASDB_PWAL_SHARDS:-}" ]; then
    PWAL_ARGS="-pwal-shards ${YASDB_PWAL_SHARDS}"
fi

# Optional: net/http/pprof on the private metrics listener, for a specific
# investigation. It adds real overhead when nonzero (see main.go), so it is
# not meant to stay set across deploys. Requires YASDB_METRICS_ADDR too.
PPROF_ARGS=""
if [ -n "${YASDB_PPROF_BLOCK_RATE:-}" ]; then
    PPROF_ARGS="$PPROF_ARGS -pprof-block-rate ${YASDB_PPROF_BLOCK_RATE}"
fi
if [ -n "${YASDB_PPROF_MUTEX_FRACTION:-}" ]; then
    PPROF_ARGS="$PPROF_ARGS -pprof-mutex-fraction ${YASDB_PPROF_MUTEX_FRACTION}"
fi

# Optional: POST /__admin/bulk-provision on the private metrics listener,
# for fast load-test setup (see internal/ds/bulkprovision.go). Off by
# default: it is a mutating, storage-filling operation. Requires
# YASDB_METRICS_ADDR too.
ADMIN_ARGS=""
if [ "${YASDB_ADMIN_BULK_PROVISION:-}" = "1" ]; then
    ADMIN_ARGS="-admin-bulk-provision"
fi

if [ -n "${YASDB_STORE:-}" ]; then
    exec yasdb -addr "$ADDR" -store "$YASDB_STORE" -db "${YASDB_DB:-yasdb}" -durability "$DURABILITY" -flush "$FLUSH" $METRICS_ARGS $COMPACTOR_ARGS $PWAL_ARGS $PPROF_ARGS $ADMIN_ARGS
else
    exec yasdb -addr "$ADDR" -data "${YASDB_DATA:-/data/yasdb}" -durability "$DURABILITY" -flush "$FLUSH" $METRICS_ARGS $COMPACTOR_ARGS $PPROF_ARGS $ADMIN_ARGS
fi
