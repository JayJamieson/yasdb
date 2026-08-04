# Benchmarks

These are load-testing tools for yasdb, built on
[`vegeta`](https://github.com/tsenart/vegeta) (a compiled Go HTTP
attacker).

Run these in-region. See
[`../deploy/loadtest/README.md`](../deploy/loadtest/README.md) for the
Fly load-generator setup — it reaches yasdb over the private 6PN
network, bypassing the public edge/proxy limits. Everything here also
runs fine against a local `yasdb` for a quick check.

## Setup

```sh
go install github.com/tsenart/vegeta@latest
(cd loadshape && go build -o "$(go env GOPATH)/bin/loadshape" .)
```

Both binaries need to be on `PATH`. `deploy/loadtest/Dockerfile` does
this automatically for the Fly load generator.

All scripts assume the target server has `-admin-bulk-provision` set.
This mounts `POST /__admin/bulk-provision` on `-metrics-addr` (see
`internal/ds/bulkprovision.go`). The scripts use it to provision their
whole stream pool in a few batched calls, instead of one HTTP request
per stream. This matters a lot above a few hundred streams.

## Smoke test

Run this first. It confirms the server and scripts agree before a real
run:

```sh
BASE_URL=http://localhost:4437 ./smoke.sh
```

This runs one PUT → POST → GET → HEAD → DELETE round trip, with plain
`curl`. No load tool is involved. If this fails, nothing else here will
produce meaningful numbers.

## Ceiling throughput: `max-throughput.sh`

This is a closed model: `WORKERS` fire back-to-back as fast as the
server answers, so throughput at that concurrency is the ceiling, not a
target rate. Sweep `STREAMS` to see how throughput scales with stream
count. Sweep `WORKERS` to confirm you are server-limited, not
client-limited — if raising `WORKERS` still raises throughput, you are
not saturated yet.

```sh
STREAMS=32768 WORKERS=800 DURATION=30s \
  BASE_URL=http://yasdb.internal:4437 \
  ADMIN_URL=http://yasdb.internal:9091/__admin/bulk-provision \
  ./max-throughput.sh
```

| Env | Default | Meaning |
| --- | --- | --- |
| `STREAMS` | 8 | pool size |
| `WORKERS` | 400 | concurrent vegeta workers |
| `DURATION` | 30s | attack duration |
| `PAYLOAD_BYTES` | 64 | append body size |

## Ramping / spiking / variable-rate shapes: `loadshape`

The four open-model shapes below all use `loadshape`
(`loadshape/main.go`). It drives vegeta through a sequence of
`{duration, target}` stages, linearly ramping the rate from wherever the
previous stage ended to its own target. This is the same model as k6's
`ramping-arrival-rate` executor, without a JS runtime. It provisions,
and for reads also seeds, its own stream pool. Each run gets a fresh
path prefix, so runs never collide.

Use it directly for a custom shape:

```sh
loadshape -admin-url http://yasdb.internal:9091/__admin/bulk-provision \
  -target-url http://yasdb.internal:4437 -streams 100 -op append \
  -start-rate 0 -stages '[{"duration":"30s","target":500},{"duration":"1m","target":500}]'
```

`-op append` posts `-payload-bytes` bodies. `-op read` does bounded
catch-up reads (`?offset=-1`) against streams seeded with
`-seed-records` records during setup. `-max-workers` caps vegeta's own
worker pool.

### `ramp-append.sh` — write throughput under a linearly increasing rate

```sh
PEAK_RATE=2000 RAMP=5m ./ramp-append.sh
```

### `spike-append.sh` — sudden burst, sustain, drop

This exercises group-commit coalescing under a step load. `STREAMS=1`
concentrates the spike on one streamer, the worst case for coalescing.

```sh
PEAK_RATE=4000 STREAMS=1 ./spike-append.sh
```

### `variable-append.sh` — sawtooth between a low and high rate

This shows how latency tracks a load that never settles.

```sh
LOW_RATE=200 HIGH_RATE=2000 ./variable-append.sh
```

### `ramp-catchup.sh` — read throughput under a ramping rate

Each request is a bounded history scan from `offset=-1`. This stresses
the SlateDB scan / per-record FFI path — see `BENCHMARKS.md`'s read
bottleneck section.

```sh
PEAK_RATE=3000 SEED_RECORDS=5000 ./ramp-catchup.sh
```

## Cleanup

Every script's streams live under a unique `/bench/<run-id>/` or
`/loadshape/<run-id>/` prefix, so runs never collide. But nothing here
tears its pool down afterward: sequential per-stream DELETEs do not
scale past a few thousand streams (see `BENCHMARKS.md`). For a real
cleanup between runs, wipe the data directory **in place** instead of
destroying and recreating the volume:

```sh
fly machine update <id> -a <app> --entrypoint "sleep infinity" --skip-health-checks -y
fly ssh console -a <app> -C "sh -c 'rm -rf /data/yasdb/* /data/yasdb/.[!.]* 2>/dev/null'"
fly machine update <id> -a <app> --entrypoint "/usr/local/bin/docker-entrypoint.sh" -y
```

## Reading results

`vegeta report` (piped from every script here) gives request rate,
latency percentiles, and success ratio for one run. For anything beyond
a single number, pull the real metrics instead of trusting either
tool's own summary. See `docs/DEPLOY.md`'s "Reading metrics from a load
test" section for how to query Fly's Prometheus API directly (CPU,
memory, disk, and yasdb's own `yasdb_append_requests_total` /
`yasdb_append_records_total` counters). These give an exact-window rate
that a load tool's own end-of-run summary cannot: `vegeta report`'s
printed rate divides by the *whole* process runtime including setup.
That is fine for `max-throughput.sh`, where setup is one bulk call, but
worth double-checking for anything with a slower setup phase.
