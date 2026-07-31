# Design notes & known gaps

Implementation decisions where the protocol or [`SPEC.md`](./SPEC.md) left a
choice, the deviations that are intentional, and what is deliberately not built
yet. Read alongside `SPEC.md` (the architecture) and [`BENCHMARKS.md`](./BENCHMARKS.md)
(the performance and concurrency analysis).

## Native library / SlateDB binding

yasdb links the SlateDB UniFFI binding (`slatedb.io/slatedb-go v0.14.1`) over cgo
and ships the matching native library at `lib/libslatedb_uniffi.so` (built for
linux/amd64 from the SlateDB commit the binding was generated against). The
`Makefile` sets the linker/loader flags; the `Dockerfile` bakes an `$ORIGIN`
rpath so the runtime binary is self-locating. To rebuild the library on another
platform, regenerate it from `slatedb-uniffi` (release build) and keep it next to
the binary.

## Durability & acknowledgement

`Stream-Next-Offset` is returned only once the append is durable, in both
durability modes (`-durability`, injected as a `committer` strategy — see
`commit.go`):

- **`sync`** (default) — the streamer group-commits: it drains a burst of queued
  appends, validates them against one pending view, and lands all accepted
  effects in a single atomic durable batch (`AwaitDurable: true`). This coalesces
  N appends into one durability round-trip. A single stream's throughput is
  bounded by `maxBurst / flush`.
- **`notifier`** — the SPEC §6 durability-notifier technique (as in s2-lite):
  write non-durably (`AwaitDurable: false`, returning immediately with an engine
  seqnum) and ack from a single background watcher of the durable watermark
  (`Db.Status().DurableSeq`, polled). The streamer keeps a writer-tail that runs
  ahead of the reader-visible durable-tail, so appends pipeline instead of
  blocking on the flush. A stream does not retire while durability callbacks are
  outstanding, so a respawn can never read a stale tail. This scales single-stream
  concurrent throughput with concurrency rather than plateauing at `maxBurst /
  flush` (see `BENCHMARKS.md`). Reader-tail publication is strictly ordered —
  every durability callback fires on the single watcher goroutine, never inline on
  the writer — so an acked offset is always immediately readable
  (read-after-acknowledge holds). An earlier ordering bug here, where an
  already-durable ack could fire on the writer goroutine and race the watcher,
  momentarily regressing the reader-visible tail, is fixed and guarded by
  `TestProbeReadAfterAck`.

Both modes pass the full suite under `-race` and the Maelstrom kafka
log-consistency and crash-injection checks.

Because `ProducerState` and stream closure ride the same `WriteBatch` as the
records, the protocol's crash window between "data appended" and "producer
state / closure updated" does not exist — stronger than §5.2.1 requires.

## Storage vertical

The `Storage` interface (`storage.go`) is the persistence boundary: `Get`,
`Commit`/`CommitAsync`, `DurableSeq`, `Scan`, `Close`. The SlateDB-backed
`slateStore` is the production implementation; the tests run against an in-memory
`fakeStore` by default (set `YASDB_TEST_BACKEND=slatedb` to run them against the
real store). Keeping the interface small keeps a second backend viable and lets
the durability strategy be tested without the native library.

`ExpiryDeadline` is modelled at the metadata level rather than with the binding's
per-key TTL, so expiry logic stays in one place (`expiry.go`).

## Judgment calls & protocol deviations

- **Read chunking (§4 offsets).** Reads are capped at `Config.MaxReadBytes`
  (default 1 MiB). For non-JSON streams a single record larger than the cap is
  delivered across reads via the offset byte component (partial-record delivery).
  `application/json` messages stay atomic (byte component always 0).
- **`Stream-Seq` scope.** Enforced per stream (one writer-seq per stream). The
  protocol lets a server choose per-writer-identity vs per-stream but requires
  documenting the choice; without auth there is no writer identity to scope by.
- **Hard delete & tombstones (§10).** `DELETE` clears the addressable metadata
  immediately (so `GET`/`HEAD` return 404 at once) and range-deletes records in
  the background against a resumable `DeletePending` cursor. It does not write a
  `PathTombstone` for a hard delete: records are keyed by the immutable
  `StreamID`, so recreating the path is safe and gets a fresh id. Consequence — a
  deleted path is immediately recreatable and reads 404 (not 410) while old
  records drain. A soft delete (a stream with outstanding forks) does write a
  tombstone and serves 410.
- **Object-store rooting.** SlateDB rejects an object-store URL with a path
  component, so a local directory cannot be `file:///abs/dir`. For local storage
  the server roots `LocalFileSystem` at `/` via `file:///` and uses the absolute
  data dir as the database path prefix (`ds.ResolveStore`). `-store` accepts real
  object-store URLs (`memory:///`, `s3://bucket`) directly.
- **`IsPrivate` / Cache-Control.** `StreamMeta.IsPrivate` drives `public` vs
  `private` cache-control but nothing sets it (no auth signal in the base
  protocol), so responses are always `public`.
- **Live-reader wake coalescing (`-live-coalesce`, default off).** Each commit
  wakes every parked live reader; under high commit-rate × many readers that is a
  thundering herd. `-live-coalesce <dur>` folds a burst of commits into one
  leading-edge-debounced wake, bounding the wake rate without dropping or
  reordering data (readers always re-read to the current tail). Default `0`
  preserves wake-on-every-commit.
- **In-memory record cache (`livecache.go`, on by default, `-no-live-cache` to
  disable).** On commit the streamer caches the payloads it just wrote so a
  caught-up live reader assembles its next chunk from RAM instead of re-scanning
  the store per-record over cgo. `tryRead` serves only the common caught-up case
  (whole records, within the bounded retained window, under the read cap) and is
  byte-for-byte identical to `readRange`; forks, mid-record byte offsets, reads
  behind the window, and over-cap reads fall back to the store scan. Populated
  only for non-fork streams while live readers are present. The retained window is
  bounded per stream (default 512 records / 256 KiB).

## Not implemented (deferred per SPEC §12)

- **`__ds` subscription APIs** → `501`. The record cache / broadcast hub is the
  natural vertical for a multiplexed subscription transport later.
- **Retention `410`** for an offset before the earliest retained position — no
  retention/compaction policy yet.
- **Rate limiting** (`429`).
- **Multi-node.** SlateDB is single-writer per database, so v1 is single-node.
  Multi-node needs sticky routing by stream path plus a writer lease; the streamer
  registry is where that would attach.
- **Read-refresh durability.** Sliding-TTL refresh on *read* is best-effort
  non-durable to avoid blocking the streamer, so a crash can lose a read-triggered
  extension. A write-triggered extension is durable.

## Testing & correctness

- **Conformance:** `@durable-streams/server-conformance-tests` v0.3.6 reports
  **332 passed / 0 failed / 6 skipped** — every non-subscription test passes; the
  6 skips are the opt-in `__ds` subscription tests.
- **Full suite under `-race`**, both durability modes
  (`YASDB_TEST_DURABILITY=notifier`) and both storage backends
  (`YASDB_TEST_BACKEND=slatedb`).
- **`TestReadHeavyStress`** — 60 concurrent readers (catch-up + long-poll + SSE)
  byte-checking a dense self-describing log against one writer, through the
  coalesced wake path, so any reorder / gap / duplicate / torn read fails.
- **Linearizability (Porcupine)** — `internal/ds/porcupine*_test.go` model each
  stream as an append-only log and check concurrent histories against the real
  server: a plain-append model, an idempotent-producer model (epoch fencing +
  exactly-once seq dedup), and a fault-injection variant (a reverse proxy severs
  connections mid-append; clients retry the same producer/epoch/seq). Each carries
  a negative-control test proving the model rejects real violations, and
  `TestProbeReadAfterAck` asserts read-after-acknowledge in both durability modes.
- **Record-cache differential guard** — `TestLiveCacheMatchesStore` and
  `FuzzLiveCacheMatchesStore` assert the cache path is byte-identical to a store
  scan across every offset (the two paths are oracles for each other). The SSE
  control frame is pinned to `json.Marshal` output by
  `TestSSEControlFrameMatchesJSON`.
- **`TestLiveFanoutCompare`** — byte-verified in-order delivery plus a
  deterministic wake-rate bound under coalescing.
- **Maelstrom** kafka log-consistency check (catch-up and live-read paths) and
  crash-injection (`test/maelstrom/run-nemesis.sh`), both durability modes — see
  [`test/maelstrom`](./test/maelstrom).
