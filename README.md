# yasdb

Yet another streaming DB — a [Durable Streams](https://github.com/durable-streams/durable-streams/blob/main/PROTOCOL.md)
server implemented as a plain `net/http` server backed by [SlateDB](https://slatedb.io),
an LSM engine that uses object storage alone for durability.

[`docs/DEPLOY.md`](./docs/DEPLOY.md) for deploying on Fly.io with provisor +
Tigris.


## Requirements

- Go 1.25+
- A C toolchain (cgo) — the SlateDB binding links a native library
- `lib/libslatedb_uniffi.so` (checked in for linux/amd64)

## Build & run

The `Makefile` wires up the cgo linker/loader flags for the bundled library:

```bash
make build
make run ARGS="-addr :4437 -data ./yasdb-data -durability notifier -live-cache-bytes 256"
```

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-addr` | `:4437` | Listen address (4437 is the Durable Streams default port) |
| `-store` | *(empty)* | Object-store URL (`memory:///`, `s3://bucket`); empty = local file store under `-data` |
| `-data` | `./yasdb-data` | Local data directory, used when `-store` is empty |
| `-db` | `yasdb` | Database path/prefix within the object store |
| `-flush` | `10ms` | WAL flush interval — the floor on single-append durable latency; `0` = engine default (100ms) |
| `-durability` | `sync` | `sync` blocks each write on durability; `notifier` pipelines appends and acks from a durable-seq watcher |
| `-notifier-poll` | `1ms` | Durable-seq poll cadence in `notifier` mode |
| `-longpoll-timeout` | `0` | How long a `live=long-poll` read blocks before returning `204`; `0` = default (25s) |
| `-live-coalesce` | `0` | Coalesce live-reader (SSE/long-poll) wakeups within this window to tame fan-out under high commit-rate × many readers; `0` = wake on every commit |
| `-no-live-cache` | `false` | Disable the in-memory recent-records cache that lets caught-up live readers avoid a store scan (correctness unchanged) |
| `-live-cache-bytes` | `0` | Per-stream cap on the recent-records cache in bytes; `0` = default (256 KiB). Lower to cut memory on many-hot-stream deployments |

## Storage backends

**Local file store** (default). Leave `-store` empty; data lands under `-data`.
The directory is created if needed and survives restarts.

```bash
make run ARGS="-data /var/lib/yasdb -addr :4437"
```

**Object store (S3 / R2 / Tigris).** Pass the bucket as `-store` and put the
prefix in `-db` — the store URL must be a bare bucket (a path in the URL is
rejected). Credentials and endpoint come from the standard AWS environment
variables (object_store conventions).

```bash
export AWS_ACCESS_KEY_ID=…  AWS_SECRET_ACCESS_KEY=…  AWS_REGION=auto
export AWS_ENDPOINT=https://fly.storage.tigris.dev      # S3-compatible endpoint
make run ARGS="-store s3://my-bucket -db yasdb -addr :4437"
```

**In-memory** (dev only, nothing persisted):

```bash
make run ARGS="-store memory:/// -addr :4437"
```

Only one process may write a given store/prefix — SlateDB is single-writer per
database. See [`docs/DEPLOY.md`](./docs/DEPLOY.md) for a full Fly.io + Tigris
deployment.

## Tuning: latency vs durability vs throughput

`-flush` sets the durability cadence: an append blocks until the next WAL flush,
so it is both the single-append latency floor and how often WAL objects are
written (which drives request cost on real object storage). `-durability` decides
whether concurrent appends pipeline. Pick from the shape of your workload:

| Goal | Flags | Why |
| --- | --- | --- |
| **Low latency** (few writers) | `-flush 1ms -durability sync` | Each append waits one short flush; group commit isn't needed at low concurrency |
| **High durable throughput** (many concurrent writers, one stream) | `-flush 10ms -durability notifier` | Appends pipeline past the flush instead of serialising on it — scales with concurrency |
| **Object-store cost / high durability** (S3, R2, Tigris) | `-flush 50ms -durability notifier` | Batches WAL PUTs to cut request cost while `notifier` keeps throughput high |

Under many live SSE/long-poll readers on a hot stream, add `-live-coalesce`
(e.g. `10ms`–`50ms`) to bound the per-commit wake fan-out. The recent-records
cache is on by default and needs no flag. Numbers behind these knobs are in
[`BENCHMARKS.md`](./BENCHMARKS.md).

## Demo playground

With the server running, open **`http://localhost:4437/__ui/`**. The single-page
playground lets you create streams, append (with optional producer headers /
`Stream-Seq` / close), read catch-up, long-poll, and watch a live **SSE** tail —
against this server, same origin.

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

The suite runs against an in-memory fake store by default (no native library
needed to execute); set `YASDB_TEST_BACKEND=slatedb` to run it against the real
SlateDB store.

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
