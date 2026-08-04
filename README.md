# yasdb

yasdb is a [Durable Streams](https://github.com/durable-streams/durable-streams/blob/main/PROTOCOL.md)
server. It runs as a plain `net/http` server. It stores data with
[SlateDB](https://slatedb.io), an LSM engine that uses only object storage
for durability.

See [`docs/DEPLOY.md`](./docs/DEPLOY.md) to deploy yasdb on Fly.io with
Tigris.


## Requirements

- Go 1.25 or later
- A C toolchain (cgo). The SlateDB binding links a native library through cgo.
- `lib/libslatedb_uniffi.so` (checked in for linux/amd64)

## Build & run

The `Makefile` sets the cgo linker and loader flags for the bundled library:

```bash
make build
make run ARGS="-addr :4437 -data ./yasdb-data"
```

Day to day, you mainly choose **where data lives**: `-store` / `-data` /
`-db` (see [Storage backends](#storage-backends) below). Notifier
durability, coalesced live-reader wakeups, and the in-memory record cache
default to the settings this project's benchmarking found fastest — no flag
needed to turn them on. The SlateDB compactor knobs stay off by default
(the engine's own defaults apply). See
[Tuning](#tuning-latency-vs-durability-vs-throughput) below for when to use
either group of settings.

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-addr` | `:4437` | Listen address (4437 is the Durable Streams default port) |
| `-store` | *(empty)* | Object-store URL (`memory:///`, `s3://bucket`). Empty means: use a local file store under `-data`. |
| `-data` | `./yasdb-data` | Local data directory, used when `-store` is empty |
| `-db` | `yasdb` | Database path/prefix within the object store |
| `-flush` | `10ms` | WAL flush interval. This is the floor on single-append durable latency. `0` means: use the engine default (100ms). |
| `-durability` | `notifier` | `notifier`: pipelines appends and acknowledges them from a durable-seq watcher (default; scales with concurrent writers). `sync`: blocks each write until it is durable. Use `sync` for lower latency with one low-concurrency writer, and for crash-recovery testing. |
| `-notifier-poll` | `1ms` | Durable-seq poll interval in `notifier` mode |
| `-longpoll-timeout` | `0` | How long a `live=long-poll` read blocks before it returns `204`. `0` means: use the default (25s). |
| `-metrics-addr` | *(empty)* | Listen address for a Prometheus `/metrics` endpoint of native SlateDB engine metrics. Empty means: disabled. Use a separate, non-public address (see `fly.toml`'s `[metrics]` block). |
| `-slatedb-log-level` | `warn` | SlateDB native (Rust-side) log level to stderr: `off\|error\|warn\|info\|debug\|trace`. Use `debug` or `trace` to diagnose stalls (see BENCHMARKS.md); both are verbose. |
| `-compactor-max-jobs` | `0` | Max concurrent SlateDB compaction jobs. `0` means: use the engine default (4). |
| `-compactor-max-subcompactions` | `0` | Max parallel workers per compaction job. `0` means: use the engine default (4). Lower this if compaction starves foreground writes. |
| `-l0-flush-parallelism` | `0` | Max concurrent L0 SST flush uploads. `0` means: use the engine default (4). |

Live-reader wake coalescing (a 5ms window) and the in-memory record cache
are always on. See [`BENCHMARKS.md`](./BENCHMARKS.md)'s "Live fan-out"
section for the measurements behind that choice. There is no flag to
disable either: correctness is the same either way, and the throughput gain
under real fan-out is large (up to 5× at 500 concurrent readers on one
stream).

The compactor knobs are still an open question; they are not a settled
default. This project's local tuning sweep (BENCHMARKS.md's "Compaction
stall under load") ran the load generator and the server on one shared box.
It found no consistent win, and the result cannot be trusted either way:
everything on the box was competing for the same cores. These knobs need
re-testing with the load generator and the server on separate Fly machines
before you can draw a real conclusion. See [`docs/DEPLOY.md`](./docs/DEPLOY.md)'s
"Reading metrics from a load test" section to pull the numbers back
afterwards.

## Storage backends

**Local file store** (default). Leave `-store` empty. Data lands under
`-data`. yasdb creates the directory if needed, and the data survives
restarts.

```bash
make run ARGS="-data /var/lib/yasdb -addr :4437"
```

**Object store (S3 / R2 / Tigris).** Pass the bucket as `-store` and put the
prefix in `-db`. The store URL must be a bare bucket; yasdb rejects a URL
that has a path. Credentials and the endpoint come from the standard AWS
environment variables (object_store conventions).

```bash
export AWS_ACCESS_KEY_ID=…  AWS_SECRET_ACCESS_KEY=…  AWS_REGION=auto
export AWS_ENDPOINT=https://fly.storage.tigris.dev      # S3-compatible endpoint
make run ARGS="-store s3://my-bucket -db yasdb -addr :4437"
```

**In-memory** (dev only; yasdb persists nothing):

```bash
make run ARGS="-store memory:/// -addr :4437"
```

Only one process may write to a given store/prefix at a time. SlateDB is
single-writer per database. See [`docs/DEPLOY.md`](./docs/DEPLOY.md) for a
full Fly.io + Tigris deployment.

## Tuning: latency vs durability vs throughput

`-flush` sets the durability interval. An append blocks until the next WAL
flush, so `-flush` is both the single-append latency floor and the rate at
which yasdb writes WAL objects (this drives request cost on real object
storage). `-durability` decides whether concurrent appends pipeline. Pick
your flags from the shape of your workload:

| Goal | Flags | Why |
| --- | --- | --- |
| **High durable throughput** (many concurrent writers, one stream) — the default | `-flush 10ms -durability notifier` | Appends pipeline past the flush instead of waiting on it one at a time. This scales with concurrency. |
| **Low latency** (a single low-concurrency writer) | `-flush 1ms -durability sync` | Each append waits on one short flush directly, with no extra notifier poll tick. Group commit gives no benefit at low concurrency. |
| **Object-store cost** (S3, R2, Tigris) | `-flush 50ms -durability notifier` | Batches WAL PUTs to cut request cost, while `notifier` keeps throughput high |

[`BENCHMARKS.md`](./BENCHMARKS.md) has the numbers behind these knobs, and
behind the always-on live-reader wake coalescing and record cache.

## Metrics

`-metrics-addr :9091` (or `YASDB_METRICS_ADDR=:9091`) starts a Prometheus
`/metrics` endpoint on its own address. It serves every metric the SlateDB
engine registers natively: write/WAL throughput, L0 SST count, stall and
backpressure counters, compaction throughput and job counts, object-store
request rate/errors/latency (as histograms), block-cache hit rate,
bloom-filter false-positive rate, and GC/expiry counts. It is off by
default and kept off the main `-addr`. On Fly, `fly.toml`'s `[metrics]`
block points at this port; Fly scrapes it over the private network (not the
public one) and surfaces it automatically in the org's built-in Grafana at
fly-metrics.net. See
[`deploy/grafana-slatedb-dashboard.json`](./deploy/grafana-slatedb-dashboard.json)
for a starter dashboard — import it, or drop it in Grafana's dashboard
provisioning path.

```bash
make run ARGS="-metrics-addr :9091"
curl localhost:9091/metrics
```

## Admin: stream listing

**`GET /__admin/streams`** lists every stream on the server: content type,
closed status, record count, fork status, and live reader count split by
long-poll vs SSE. The list is cursor-paginated (`?cursor=`/`?limit=`,
default page size 50, capped at 500):

```bash
curl "localhost:4437/__admin/streams?limit=50"
# {"streams":[{"path":"/demo/chat","contentType":"application/json","closed":false,
#              "records":42,"isFork":false,"readers":{"longPoll":0,"sse":1}}],
#  "nextCursor":"..."}
```

For a stream with recent activity, reader counts and record counts reflect
the resident streamer's live in-memory state. For a dormant stream, they
fall back to durable state; a dormant stream by definition has no parked
readers. This endpoint adds no new read path for stream contents:
offset-based browsing reuses the existing catch-up `GET <path>?offset=`
endpoint and its `Stream-Next-Offset` cursor.

## State Protocol & live queries

`internal/state` implements the [Durable Streams State Protocol](https://github.com/durable-streams/durable-streams/blob/main/packages/state/STATE-PROTOCOL.md).
This is a JSON message convention (`type`/`key`/`value`/`headers.operation`)
layered on ordinary `application/json` streams. It gives change events
(insert/update/delete) and control events (snapshot boundaries, reset)
enough meaning to materialize into queryable entity state. It needs no
changes to the base stream engine; see `DESIGN.md` for why.

**`GET /__state/<stream-path>`** is a server-side convenience built on that
protocol: it replays a stream's full history through a
`state.Materializer` and returns the current materialized state, grouped
by entity type, plus a `streamNextOffset` to resume live-tailing from. Use
it from a non-JS client, or anywhere you want to `curl` the current state
without replaying raw history yourself.

```bash
curl localhost:4437/__state/demo/chat
# {"generatedAt":"...","streamNextOffset":"...","collections":{"user":[...],"message":[...]}}
```

## Quick tour with curl

```bash
# create a JSON stream
curl -X PUT localhost:4437/orders -H 'Content-Type: application/json'

# append a batch (flattened into two messages) and one more
curl -X POST localhost:4437/orders -H 'Content-Type: application/json' -d '[{"id":1},{"id":2}]'
curl -X POST localhost:4437/orders -H 'Content-Type: application/json' -d '{"id":3}'

# read from the start
curl 'localhost:4437/orders?offset=-1'      # -> [{"id":1},{"id":2},{"id":3}]

# live tail via SSE
curl -N 'localhost:4437/orders?offset=now&live=sse'

# close (EOF) and delete
curl -X POST   localhost:4437/orders -H 'Stream-Closed: true'
curl -X DELETE localhost:4437/orders
```

## Test

By default, the suite runs against an in-memory fake store, so it needs no
native library to execute. Set `YASDB_TEST_BACKEND=slatedb` to run it
against the real SlateDB store instead.

```bash
make test ARGS="-race"                                   # full suite, sync durability, fake store
YASDB_TEST_DURABILITY=notifier make test ARGS="-race"    # notifier durability
YASDB_TEST_BACKEND=slatedb make test ARGS="-race"        # against the real SlateDB store
make bench                                               # throughput / allocation benchmarks (real store)
make fuzz                                                # property: cache bytes == store bytes (Ctrl-C to stop)
MAELSTROM=/path/to/maelstrom/maelstrom make maelstrom DUR=notifier        # Jepsen log-consistency check
MAELSTROM=/path/to/maelstrom/maelstrom make maelstrom DUR=notifier READ=long-poll  # …over the live-read + cache path
MAELSTROM=/path/to/maelstrom/maelstrom make maelstrom-chaos DUR=notifier  # crash-injection durability check
```
