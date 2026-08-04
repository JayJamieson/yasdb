# Design notes & known gaps

## Native library / SlateDB binding

yasdb links the SlateDB UniFFI binding (`slatedb.io/slatedb-go v0.14.1`)
over cgo. It ships the matching native library at
`lib/libslatedb_uniffi.so`, built for linux/amd64 from the SlateDB commit
the binding was generated against. The `Makefile` sets the linker/loader
flags. The `Dockerfile` bakes an `$ORIGIN` rpath, so the runtime binary
finds the library on its own. To rebuild the library on another platform,
regenerate it from `slatedb-uniffi` (release build) and keep it next to the
binary.

## Durability & acknowledgement

yasdb returns `Stream-Next-Offset` only once the append is durable, in both
durability modes. `-durability` selects the mode, injected as a
`committer` strategy — see `commit.go`:

- **`sync`** (default). The streamer group-commits: it drains a burst of
  queued appends, validates them against one pending view, and lands all
  accepted effects in a single atomic durable batch (`AwaitDurable:
  true`). This coalesces N appends into one durability round-trip. A
  single stream's throughput is bounded by `maxBurst / flush`.
- **`notifier`**. This is the durability-notifier technique (as in
  s2-lite): yasdb writes non-durably (`AwaitDurable: false`), which
  returns immediately with an engine seqnum, and acknowledges from a
  single background watcher of the durable watermark
  (`Db.Status().DurableSeq`, polled). The streamer keeps a writer-tail
  that runs ahead of the reader-visible durable-tail, so appends pipeline
  instead of blocking on the flush. A stream does not retire while
  durability callbacks are outstanding, so a respawn can never read a
  stale tail. This scales single-stream concurrent throughput with
  concurrency, instead of plateauing at `maxBurst / flush` (see
  `BENCHMARKS.md`). Reader-tail publication is strictly ordered: every
  durability callback fires on the single watcher goroutine, never inline
  on the writer. So an acknowledged offset is always immediately
  readable (read-after-acknowledge holds). An earlier ordering bug let an
  already-durable acknowledgement fire on the writer goroutine and race
  the watcher, which could momentarily regress the reader-visible tail.
  That bug is fixed, and `TestProbeReadAfterAck` guards against it.

Both modes pass the full suite under `-race` and the Maelstrom kafka
log-consistency and crash-injection checks.

`ProducerState` and stream closure ride the same `WriteBatch` as the
records. So the protocol's crash window between "data appended" and
"producer state / closure updated" does not exist. This is stronger than
§5.2.1 requires.

## Storage vertical

The `Storage` interface (`storage.go`) is the persistence boundary: `Get`,
`Commit`/`CommitAsync`, `DurableSeq`, `Scan`, `Close`. The SlateDB-backed
`slateStore` is the production implementation. By default, tests run
against an in-memory `fakeStore` (set `YASDB_TEST_BACKEND=slatedb` to run
them against the real store instead). Keeping the interface small keeps a
second backend viable, and lets the durability strategy be tested without
the native library.

`ExpiryDeadline` is modelled at the metadata level, not with the binding's
per-key TTL. This keeps all expiry logic in one place (`expiry.go`).

## State Protocol

`internal/state` implements the [Durable Streams State Protocol](https://github.com/durable-streams/durable-streams/blob/main/packages/state/STATE-PROTOCOL.md).
This is a message-format convention (`type`/`key`/`value`/`headers.operation`
change messages, `headers.control` control messages) layered on top of
ordinary `application/json` streams. It is not a new wire protocol.
`internal/ds` already handles arbitrary JSON message streams correctly
(array flattening on append, single-array wrapping on read, per
`json.go`), so nothing in the stream engine needed to change to support
it. `internal/state.Message` and `internal/state.Materializer` are a
standalone, reusable implementation of message validation (§5) and
materialization (§6).

`GET /__state/<path>` (`state.go`, wired in `main.go` alongside
`/__health`) is a convenience layer built on top of that, not a protocol
requirement: it replays a stream's full history through a fresh
`Materializer` and returns the current entity state grouped by type, plus
the `Stream-Next-Offset` to resume live-tailing from via the existing
`?live=sse` endpoint. It is implemented as a self-request: a real `GET
?offset=-1` constructed against `Server.ServeHTTP` through a small
hand-rolled `http.ResponseWriter` recorder. This keeps it outside
`internal/ds`, so it depends only on `Server`'s public HTTP surface, the
same way any other client would. It is meant for a non-JS client or ad-hoc
`curl` debugging.

## Metrics

SlateDB's UniFFI binding ships a `DefaultMetricsRecorder`, attached in
`openStore` via `DbBuilder.WithMetricsRecorder`. The Rust engine registers
about 120 Counter/Gauge/UpDownCounter/Histogram metrics against it natively
(write/WAL path, L0/compaction health, object-store calls, block cache,
bloom filters, GC). `internal/ds/metrics.go` snapshots it and hand-rolls a
Prometheus text-exposition encoder (`Server.WriteMetrics`), instead of
pulling in `client_golang`. The reason: the data is already-computed
values SlateDB handed us, not instruments this process owns and
increments. So there is nothing for a metrics *registry* to do here, only
a format conversion. The conversion has to do two things itself: strip
`.` from Prometheus metric names (SlateDB's are dotted, e.g.
`slatedb.db.request_count`), and turn SlateDB's per-bucket exclusive
histogram counts into the cumulative counts Prometheus's
`_bucket{le=...}` series requires.

Metrics are served on their own port (`-metrics-addr`/
`YASDB_METRICS_ADDR`, off by default), not a path on `-addr`. This matches
Fly's own convention for `[metrics]` in `fly.toml`: scraped over the
private network, never exposed alongside the public stream keyspace.
`MetricsProvider` (storage.go) is a separate small interface that
`Storage` implementations opt into; it is not a method on `Storage`
itself. `fakeStore` (the test suite's default backend) does not implement
it, and `Server.WriteMetrics` just returns an error in that case. This
keeps the core interface free of a concern only one backend has.

## Judgment calls & protocol deviations

- **Read chunking.** Reads are capped at
  `Config.MaxReadBytes` (default 1 MiB). For non-JSON streams, a single
  record larger than the cap is delivered across reads via the offset
  byte component (partial-record delivery). `application/json` messages
  stay atomic (byte component always 0).
- **`Stream-Seq` scope.** Enforced per stream (one writer-seq per
  stream). The protocol lets a server choose per-writer-identity vs
  per-stream, but requires documenting the choice. Without auth, there
  is no writer identity to scope by.
- **Hard delete & tombstones (§10).** `DELETE` clears the addressable
  metadata immediately, so `GET`/`HEAD` return 404 at once, and
  range-deletes records in the background against a resumable
  `DeletePending` cursor. It does not write a `PathTombstone` for a hard
  delete: records are keyed by the immutable `StreamID`, so recreating
  the path is safe and gets a fresh id. As a result, a deleted path is
  immediately recreatable and reads 404 (not 410) while old records
  drain. A soft delete (a stream with outstanding forks) does write a
  tombstone and serves 410.
- **Object-store rooting.** SlateDB rejects an object-store URL with a
  path component, so a local directory cannot be `file:///abs/dir`. For
  local storage, the server roots `LocalFileSystem` at `/` via `file:///`
  and uses the absolute data dir as the database path prefix
  (`ds.ResolveStore`). `-store` accepts real object-store URLs
  (`memory:///`, `s3://bucket`) directly.
- **`IsPrivate` / Cache-Control.** `StreamMeta.IsPrivate` drives `public`
  vs `private` cache-control, but nothing sets it: the base protocol has
  no auth signal. So responses are always `public`.
- **Live-reader wake coalescing (`Config.LiveCoalesceWindow`, always on
  in the `yasdb` binary — see `main.go`'s `defaultLiveCoalesceWindow`).**
  Each commit wakes every parked live reader. Under a high commit rate
  with many readers, that becomes a thundering herd. A nonzero window
  folds a burst of commits into one leading-edge-debounced wake, which
  bounds the wake rate without dropping or reordering data (readers
  always re-read to the current tail). The zero value (wake on every
  commit, no coalescing) is still a real `Config` option: `TestLiveFanoutCompare`
  uses it as the "v1"/thundering-herd baseline, and any caller of the
  `internal/ds` package can use it directly. It is just not exposed as a
  flag on the shipped binary, since BENCHMARKS.md found a small window to
  be a strict win with no downside.
- **In-memory record cache (`livecache.go`, on by default;
  `Config.DisableLiveCache` is an escape hatch for callers of the
  package, not a flag on the binary).** On commit, the streamer caches
  the payloads it just wrote, so a caught-up live reader assembles its
  next chunk from RAM instead of re-scanning the store per-record over
  cgo. `tryRead` serves only the common caught-up case (whole records,
  within the bounded retained window, under the read cap), and it is
  byte-for-byte identical to `readRange`. Forks, mid-record byte
  offsets, reads behind the window, and over-cap reads fall back to the
  store scan. The cache is populated only for non-fork streams, and only
  while live readers are present. The retained window is bounded per
  stream (default 512 records / 256 KiB).

## Not implemented

- **`__ds` subscription APIs** return `501`. The record cache /
  broadcast hub is the natural vertical for a multiplexed subscription
  transport later.
- **Retention `410`** for an offset before the earliest retained
  position — yasdb has no retention/compaction policy yet.
- **Rate limiting** (`429`).
- **Multi-node.** SlateDB is single-writer per database, so v1 is
  single-node. Multi-node needs sticky routing by stream path plus a
  writer lease; the streamer registry is where that would attach.
- **Read-refresh durability.** Sliding-TTL refresh on *read* is
  best-effort non-durable, to avoid blocking the streamer, so a crash
  can lose a read-triggered extension. A write-triggered extension is
  durable.

## Testing & correctness

- **Conformance:** `@durable-streams/server-conformance-tests` v0.3.6
  reports **332 passed / 0 failed / 6 skipped**. Every non-subscription
  test passes; the 6 skips are the opt-in `__ds` subscription tests.
- **Full suite under `-race`**, both durability modes
  (`YASDB_TEST_DURABILITY=notifier`) and both storage backends
  (`YASDB_TEST_BACKEND=slatedb`).
- **`TestReadHeavyStress`** runs 60 concurrent readers (catch-up +
  long-poll + SSE) that byte-check a dense self-describing log against
  one writer, through the coalesced wake path. Any reorder, gap,
  duplicate, or torn read fails this test.
- **Linearizability (Porcupine).** `internal/ds/porcupine*_test.go`
  model each stream as an append-only log and check concurrent histories
  against the real server: a plain-append model, an idempotent-producer
  model (epoch fencing + exactly-once seq dedup), and a fault-injection
  variant (a reverse proxy severs connections mid-append; clients retry
  the same producer/epoch/seq). Each carries a negative-control test
  that proves the model rejects real violations, and
  `TestProbeReadAfterAck` asserts read-after-acknowledge in both
  durability modes.
- **Record-cache differential guard.** `TestLiveCacheMatchesStore` and
  `FuzzLiveCacheMatchesStore` assert the cache path is byte-identical to
  a store scan across every offset (the two paths are oracles for each
  other). `TestSSEControlFrameMatchesJSON` pins the SSE control frame to
  `json.Marshal` output.
- **`TestLiveFanoutCompare`** checks byte-verified in-order delivery,
  plus a deterministic wake-rate bound under coalescing.
- **Maelstrom** runs a kafka log-consistency check (catch-up and
  live-read paths) and a crash-injection check
  (`test/maelstrom/run-nemesis.sh`), in both durability modes. See
  [`test/maelstrom`](./test/maelstrom).
