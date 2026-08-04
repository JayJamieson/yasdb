# Performance & concurrency notes

Run: `make bench` (adds `-benchmem`). Machine: 2 vCPU AMD EPYC, in-memory
object store unless noted, 11-byte records.

## Results

| Benchmark | ns/op | throughput | B/op | allocs/op |
| --- | --- | --- | --- | --- |
| AppendSerial, flush=100ms | 101,447,308 | **9.9 appends/s** | 5,373 | 63 |
| AppendSerial, flush=10ms | 11,198,590 | **89 appends/s** | 4,745 | 57 |
| AppendSerial, flush=1ms | 2,199,938 | **455 appends/s** | 4,692 | 57 |
| AppendBurst (128 producers, 1 stream), flush=100ms | 1,635,563 | **611 appends/s** | 2,171 | 19 |
| AppendBurst (128 producers, 1 stream), flush=10ms | 176,422 | **5,668 appends/s** | 2,151 | 18 |
| AppendBurst, **file store** (fsync), flush=10ms | 175,869 | **5,686 appends/s** | 2,151 | 18 |
| ReadCatchup (100 records/req) | 323,314 | **3,093 reads/s** | 71,100 | 1,848 |

## Where the bottlenecks are

**1. Append latency equals the SlateDB WAL flush interval.** An
`AwaitDurable` write blocks until the next flush. So serial durable-append
latency is about equal to `flush_interval` (100 ms default → 9.9/s; 10 ms
→ 89/s; 1 ms → 455/s). This is now configurable with the `-flush` flag
(default **10 ms**) and `OpenStore(..., flushInterval)`. A lower value
gives lower latency but more frequent object-store writes, which matters
for S3 cost. It is a knob, not a free win.

**2. Group commit removes the throughput penalty.** With 128 concurrent
producers on one stream, the streamer folds queued appends into a single
durable batch (up to `maxBurst`=64). This gives **611 appends/s at a 100
ms flush** — a **~62×** speed-up over the 9.9/s serial rate at the same
flush — and **5,668/s at 10 ms**. This is the spec's pipelining benefit,
working: throughput is no longer divided by the flush interval. The
append path is **not CPU-bound**. The CPU profile shows only about 13%
utilization during the burst, almost all of it in `libc` /
`libslatedb_uniffi` / `runtime.cgocall` (the durability machinery).
yasdb's own `planAppend`/`processBurst` code is a rounding error.

**3. The durability backend is not the limit at these rates.** The
on-disk file store (real fsync) matched the in-memory store (**5,686 vs
5,668/s**). The WAL flush *cadence* and coalescing dominate, not disk I/O.

**4. The read bottleneck is the SlateDB binding's per-record FFI cost.**
A 100-record catch-up read allocates **1,848 objects (about 18/record)**.
The alloc profile attributes **87%** of them to `uniffiRustCallAsync` and
the RustBuffer deserialisation (`FfiConverterOptionalKeyValue.Read`,
`encoding/binary.Read`, `bytes.NewReader`) inside `DbIterator.Next()`.
Every record crossed the CGo boundary as an async UniFFI call.

### Tunables

- `-flush <dur>` — durability latency floor (default 10 ms).
- `-durability sync|notifier` — see below.
- `maxBurst` (const, 512) — the max appends folded per group commit, and
  the size of every stream's `reqs` channel buffer (`appendReq` is 80
  bytes). yasdb allocates this **per stream, unconditionally, at spawn**.
  So it must be sized for resident-stream *count*, not single-stream
  throughput. It was raised to 4096 once during a single-hot-stream
  sweep (320 KiB there — nothing), and stayed at that value. The problem
  only surfaced when a 32768-stream production run's memory climbed to
  82% of an 8GB box: 4096 × 80B × 32768 streams = 10 GiB of channel
  buffer alone. It is now reverted to 512 (about 1.2 GiB at the same
  scale, with no throughput cost). Check the count × size product before
  raising this value again.

## Connected-reader capacity (read fan-out)

The read numbers above measure **active** reads: clients pulling bytes as
fast as they can, which is CPU-bound (see §4 above — per-record FFI
dominates, and about 2 cores saturate immediately). A separate question
is how many **connected but idle** readers a single process can hold —
long-poll / SSE clients parked waiting for the next record. That ceiling
is set by **memory**, not CPU, ports, threads, or file descriptors.

Measured by opening N concurrent SSE readers (`?offset=now&live=sse`)
against one in-memory server (`-flush 5ms`) and holding them open (load
generator in `readbench -hold`, multi-source-IP dialing to get past the
client's ~28k single-IP ephemeral-port limit):

| connected SSE readers | server RSS | server threads | failures |
| --- | --- | --- | --- |
| 0 (baseline) | ~16 MB | — | — |
| 5,000 | 163 MB | 13 | 0 |
| 15,000 | 453 MB | 16 | 0 |

This gives a clean linear fit: **about 29 KB of server RSS per connected
reader, plus about 18 MB base.**

**What does *not* cap it:**

- **Threads.** SSE/long-poll waiters park on a Go **channel**
  (`waiterChan`), not an OS thread. So the process held 15k+ readers at
  only **16 OS threads**; the Go runtime's ~10k-thread crash ceiling is
  never approached. (This is the payoff of not blocking a
  goroutine-per-connection on a cgo call.)
- **File descriptors.** `ulimit -n` is 524,288 here — orders of magnitude
  above the RAM ceiling.
- **Ephemeral ports.** About 28k per source IP is a *client-side* limit,
  worked around here with multiple source IPs. The server binds one
  listening socket, so this limit does not apply to it.

**Where it fails over: RAM.** At about 29 KB/reader, the ceiling is
memory exhaustion. On a box with about 2.5 GB usable to the server alone,
this extrapolates to **about 80k connected readers** before OOM, with no
other resource intervening first. That number is *extrapolated* from the
linear fit, not measured to the wall. Measuring the true tip-over point
requires the load generator on a **separate** machine, because a
co-located client costs its own per-connection memory and the two
compete for the same RAM. (On the 3.8 GB test box, the *joint*
client+server ceiling is roughly half the server-only figure, and
driving toward it OOMs the whole VM.) Rule of thumb for capacity
planning: **budget about 30 KB of server RAM per concurrent connected
reader**; everything else scales out of the way.

**Caveat — this RAM ceiling is for *idle* readers.** The numbers above
were measured with readers parked on a stream that was **not being
written**. The moment a stream is *actively written*, each commit wakes
every parked reader (fan-out). For a hot stream, that CPU cost — not RAM
— becomes the first wall, at a reader count well below the RAM
extrapolation. See the next section.

## Live fan-out: the wake cost and coalescing (`-live-coalesce`)

A parked long-poll/SSE reader is cheap until the stream is written. On
each commit, the streamer closes a shared broadcast channel, waking
**every** parked reader, and each woken reader re-scans the store for
the new records (per-record cgo/FFI, see §4 above). So a commit that
appends one record to a stream with N live readers costs O(N) wakeups
**and** O(N) store scans — a thundering herd. Under a high commit rate
with many readers, this outruns the cores: with N readers and `c` CPU
per woken reader, the live path stays stable only while `commit_rate × N
× c < cores`. Above that threshold, the herd never drains between
commits and throughput collapses — a metastable cliff, not a transient
spike. On this 2-core box, that threshold is a low commit rate once N
reaches the tens of thousands, which is why the *written*-stream reader
ceiling sits far below the idle RAM ceiling.

**`-live-coalesce <dur>`** (default `0`, wake on every commit — the
original behavior) bounds the wake rate with leading-edge debounce: a
commit after a quiet gap at or above the window wakes readers
immediately (no added latency for low-rate streams), and commits inside
the window fold into a single trailing wake. It never drops or reorders
data: readers always re-read to the current tail, so one wake after a
burst delivers every accumulated record in order. It trades up to
`window` of extra delivery latency on hot streams for a bounded wake
rate.

Measured with a fixed pool of live SSE readers on one written stream, a
single writer landing commits, in-memory store (`BenchmarkLiveFanout`,
non-`-race`):

| readers | mode | ns/commit (fan-out) | broadcasts / commit | deliver/s |
| --- | --- | --- | --- | --- |
| 100 | `0` (v1) | 3,121,156 | 1.00 | 32,039 |
| 100 | `10ms`   | 2,454,152 | 0.235 | **40,747** |
| 500 | `0` (v1) | 15,043,890 | 1.00 | 33,236 |
| 500 | `10ms`   | 14,184,225 | 0.87 | 35,250 |
| 500 | `50ms`   | 5,354,542 | 0.107 | **93,379** |

Two takeaways:

1. **Coalescing works and compounds with fan-out.** At 500 readers, a
   50ms window folds about 9 commits per wake: **9.4× fewer broadcasts
   (1.00→0.11 per commit), 2.8× lower per-commit latency, 2.8× more
   delivery throughput.** It also frees the *writer*: under `-race`
   contention, the writer's own append phase dropped from 3.9 s to 1.2 s
   once it stopped competing with the herd.
2. **The window must exceed the commit interval.** At 500 readers, the
   per-commit fan-out already costs about 15 ms. So commits naturally
   space out past a 10 ms window, and the leading edge fires on nearly
   every one (0.87/commit — little benefit). Widening to 50 ms recovers
   the win. Tune the window to the fan-out and acceptable latency, not
   to a fixed number.

Coalescing bounds the wake *rate*, but each wake still made every reader
re-scan the store per-record over cgo. The **in-memory record cache**
(the "broadcast hub") removes that: on commit, the streamer caches the
bytes it just wrote, and a caught-up reader assembles its next chunk
from RAM instead of scanning the store. A commit becomes one in-memory
fan-out instead of N cgo re-scans. It is on by default (non-fork streams
with live readers); `-no-live-cache` disables it. Correctness is
identical: `tryRead` serves only the common caught-up case (whole
records, fully in the retained window, under the read cap) and returns
bytes byte-for-byte equal to `readRange`. Everything else (forks,
mid-record byte offset, behind the window, over the cap) falls back to
the store scan.

Same fan-out, coalesce window fixed at 10ms, cache off vs on
(`BenchmarkLiveFanoutHub`):

| readers | cache | ns/commit | cache-hits/commit | deliver/s |
| --- | --- | --- | --- | --- |
| 100 | off | 2,391,825 | 0 | 41,809 |
| 100 | on  | 2,204,446 | 21.5 | 45,363 |
| 500 | off | 14,122,803 | 0 | 35,404 |
| 500 | on  | 2,786,231 | 131 | **179,454** |

At 500 readers the cache is **5.1× faster (14.1 → 2.8 ms/commit) and
5.1× more delivery throughput (35k → 179k/s)**, with about 100% of live
reads served from RAM. It also *compounds* with coalescing: cheaper
reads let commits land faster, so more fold per window (wakeups dropped
from 521 to 158 at 500 readers). Per-hot-stream memory is bounded and
small — the retained window defaults to **512 records / 256 KiB**
(`Config.LiveCacheMaxRecords` / `LiveCacheMaxBytes`, `-live-cache-bytes`),
which a caught-up reader never outruns (shrinking it 8× from an earlier
2 MiB left the hit rate and throughput unchanged). It is **per stream,
not per reader**: one cache serves all of a stream's readers, and
payloads are referenced, not copied (the commit's `marshalRecord` buffer
is already a private write-once copy). Write-only streams, streams with
no live readers, and forks keep no cache at all.

**Still on the store scan:** catch-up (`?offset=` one-shot) reads,
forks, mid-record partial reads, and readers behind the retained window
— none of these are the live fan-out hot path. Correctness coverage:
`TestLiveCacheMatchesStore` (differential — cache output equals
`readRange` byte-for-byte across every offset, both binary and JSON),
`FuzzLiveCacheMatchesStore` (property version — the fuzzer picks content
type / cap / record sizes, and checks `tryRead` equals `readRange`
wherever the cache serves; it found a real cap-boundary bug, a trailing
zero-length record at `total == limit`, now fixed by serving only
strictly under the limit), `TestLiveFanoutCompare` (byte-verified order,
deterministic wake bound, no regression), and `TestReadHeavyStress`
(60-reader byte-level check through both the coalesced wake path and the
cache). All of these run under `-race` in both durability modes, plus
`make maelstrom READ=long-poll`.

### Read-path allocation profile (concurrent writers)

Profiling the fan-out (200 writers group-committing into one stream that
500 readers tail) put the CPU where it should be: the read path is
**socket-I/O bound, not cgo-bound** (`runtime.cgocall` dropped from 35%
to 1% vs cache off). But it showed SSE **serialization** dominating
server allocation, and the hot spot *shifts with batch size*: the
control frame dominates under many small commits, the data frame under
big batches. Four fixes made the SSE read-serving path essentially
allocation-free (byte output unchanged; guarded by the byte-identity
tests):

1. hoist `[]byte("literal")` framing constants to package vars;
2. `writeSSEData` fast-path — write the payload bytes verbatim when it
   has no CR/LF (the common case), skipping `string`/`Split`/`[]byte(line)`;
3. hand-roll `Offset.String` (no `fmt.Sprintf`);
4. hand-roll the control frame (no `json.Marshal`) — safe because every
   value is escape-free (offset digits/`_`, decimal cursor; pinned by
   `TestSSEControlFrameMatchesJSON`); plus the SSE handler reuses a
   per-connection body + control scratch buffer.

200-writer heap, before → after: `writeSSEData` **398 MB → under 2 MB**,
`tryRead` body **196 MB → 6.5 MB**, control-frame `json.Marshal`/`fmt`
**about 30 MB → about 0**. Total allocations **1043 MB → 409 MB**, with
the remainder being the co-located load client and the inherent
write-durability path (`marshalRecord` + FFI commit). Throughput rose,
because of less GC. A multi-stream variant (writers × streams × readers)
confirmed the per-stream cache stays tiny in practice — about **5
KB/stream** (far under the 256 KiB cap, since a caught-up reader's
window holds only a few commits). So cache memory scales at a few KB per
hot stream (50 hot streams is about 195 KB total).

## Compaction stall under load

**Symptom (reported from Fly.io load testing):** periodic latency
stutters under sustained write load, not a sustained or flat ceiling.

**Root cause found** via SlateDB's native (Rust-side) logging —
`-slatedb-log-level`, which routes straight to stderr with no
FFI-callback overhead per line, so it does not perturb the load test it
is diagnosing (`InitLogging(level, nil)`; `internal/ds/storage.go`).
Reproduced locally against real MinIO, so the object-store hop behaves
like S3/Tigris rather than the local-file shortcut: `-durability
notifier -flush 50ms`, `PAYLOAD_BYTES=4096`, 50 VUs, 4 streams, about 900
req/s sustained for 5 minutes (the load test used at the time,
`max-throughput.js`, predates this repo's later switch to vegeta — see
"Stream-count scaling" below). Full raw log and load-test output are
saved under
[`docs/investigations/compaction-stall-2026-08-01.log`](./docs/investigations/compaction-stall-2026-08-01.log)
and
[`...-k6-summary.txt`](./docs/investigations/compaction-stall-2026-08-01-k6-summary.txt).

**Findings:**

- p50/p90/p95 stayed flat for the entire run (about 51/59/65 ms), but
  max latency hit **1.79 s**, once. The worst ~25 requests, across every
  stream in the pool, landed in the **same 15 ms window**. This was not
  gradual degradation; it was one synchronized stall that released
  everything at once.
- The logs pinpoint the cause: an L0→sorted-run compaction had just
  finished, and the compactor immediately scheduled a **tiered
  compaction merging all 4 accumulated sorted runs into one**
  (`estimated_source_bytes=992.69 MB`), run as 4 concurrent
  subcompactions (`worker.max_subcompactions` default).
- **`slatedb_db_backpressure_count` and `slatedb_db_l0_stall_count`
  stayed at zero throughout the stall.** This is the important part: it
  is *not* SlateDB's deliberate write-throttle doing its job (that would
  show up in those counters). It is uncontended resource contention. The
  compaction's 4 parallel subcompactions competed with the WAL flush for
  the same CPU and object-store PUT bandwidth, with nothing arbitrating
  between them.
- This is the classic tiered-compaction shape: sorted runs accumulate
  until a threshold, then get merged **all at once** in one big job
  rather than smaller incremental merges. The bigger the accumulated
  backlog, the bigger — and more disruptive — the eventual merge.
- A smaller/shorter run at the same request rate but tiny (64 B)
  payloads never crossed the L0 SST size threshold at all: zero bytes
  compacted, no stall. **This is a data-volume-triggered failure mode,
  not a request-rate one.** A low-throughput/small-payload load test
  will not reproduce it, even at the same requests/sec.

**Fix:** `openStore` previously configured nothing beyond
`flush_interval`; every other SlateDB engine knob ran on bare defaults.
`StoreTuning` (`internal/ds/storage.go`) now also exposes
`-compactor-max-jobs`, `-compactor-max-subcompactions`, and
`-l0-flush-parallelism` (all via `Settings.Set("dotted.path",
jsonValue)`, the same mechanism `flush_interval` already used), trading
slower catch-up compaction for less foreground disruption during a big
merge.

### Baseline: S3 vs. disk, default vs. tuned compactor

Same workload as the repro above (`-durability notifier -flush 50ms`,
`PAYLOAD_BYTES=4096`, 50 VUs, 4 streams, 5 min, about 2.1 GB written per
run — enough to trigger real compaction every time), one run per cell,
sweeping `-compactor-max-subcompactions` at 1, the engine default (4),
and 8 (a SlateDB maintainer suggested 8 "may be helpful for disk storage
and probably S3" — tested rather than taken on faith). Full load-test
output, final `/metrics` snapshot, and engine logs for every cell are
saved under
[`docs/investigations/compactor-tuning-baseline-2026-08-01/`](./docs/investigations/compactor-tuning-baseline-2026-08-01/).

| backend | subcompactions | req/s | avg | p90 | p95 | max | SSTs written |
| --- | --- | --- | --- | --- | --- | --- | --- |
| S3 (MinIO) | 1 | 932/s | 53.3ms | 60.2ms | 65.9ms | 1.15s | 8 |
| S3 (MinIO) | 4 (default) | 931/s | 53.3ms | 60.0ms | 65.1ms | **489.6ms** | 20 |
| S3 (MinIO) | 8 | 924/s | 53.8ms | 59.1ms | 63.8ms | 2.43s | 24 |
| disk (local file) | 1 | 974/s | 50.9ms | 59.5ms | 63.1ms | 387.7ms | 8 |
| disk (local file) | 4 (default) | 962/s | 51.6ms | 59.5ms | 63.2ms | 571.9ms | 20 |
| disk (local file) | 8 | 964/s | 51.5ms | 59.8ms | 63.7ms | **347.9ms** | 24 |

**Direct answer: no, this knob did not give a consistent stall
reduction.** The picture is genuinely mixed, not a clean "yes it works."
Six runs, one variable swept three ways, on two backends:

- **Disk backend: the maintainer's hint holds up, though not as a clean
  "more is better" curve.** `8` gave the best result (347.9ms, a real
  about 40% cut from the default's 571.9ms), and `1` also beat the
  default (387.7ms). So both directions away from `4` helped, with `8`
  helping most. Read this cautiously — one run each — but it is
  consistent with local disk I/O not being network-bound: more
  parallelism can finish the merge faster without the "too many
  concurrent requests competing for one client" cost that seems to have
  hurt S3 below.
- **S3 (MinIO) backend: the opposite happened.** `4` (the engine
  default) was the *best* of the three, and `8` was the *worst result in
  the entire investigation*: 2.43s max, worse than doing nothing. Going
  from `4` to `8` did not shorten the contention window here; it just
  put more concurrent requests through the same client at once.
- **This S3 result is not a verdict on the maintainer's claim,
  specifically because of what was tested against.** MinIO on loopback
  has near-zero network latency. The classic argument for higher request
  concurrency against an object store is *hiding* per-request latency
  behind more in-flight requests, and there is essentially no latency to
  hide against a local MinIO instance on the same machine. Real
  S3/Tigris has actual network round-trip latency, where that mechanism
  could plausibly work the way the maintainer described. This test setup
  is close to a worst case for demonstrating the S3 half of their
  suggestion, not a fair read of it.

**Caveats that apply to the whole table, not just S3:** this is one run
per cell. A single 5-minute run's `max` is one sample, not a
distribution, and is not enough to fully separate a real effect from
where the compaction backlog happened to cross its trigger during that
particular window. Also, this VM ran MinIO, the load generator, and
yasdb all at once, competing for the same cores the compaction/flush
contention is actually about — the opposite of an isolated production
machine.

**What this baseline establishes confidently regardless:** none of the
six configurations ever tripped `slatedb_db_backpressure_count` or
`slatedb_db_l0_stall_count` (both stayed at 0 in every run), all six
sustained about 925-975 req/s with **zero errors**, and the flag
demonstrably does what it says mechanically (SST count scales with the
subcompaction count: 8→20→24 output SSTs at 1→4→8 subcompactions).

**Recommendation:** on disk-backed deployments,
`-compactor-max-subcompactions 8` has real local evidence behind it now,
not just a maintainer's word, so it is worth adopting. On S3/Tigris,
this specific test neither validates nor refutes the suggestion. The
right next step is the same sweep run against real S3/Tigris (not
MinIO) on the actual target Fly machine, before picking a production
value one way or the other.

## Durability modes (`-durability`)

Both modes return `Stream-Next-Offset` only once the data is durable.
They differ in whether the append **blocks** on durability:

- **`sync`** (default) — each group commit blocks on `AwaitDurable`.
  This is simple; a stream's throughput is capped at `maxBurst / flush`,
  because the streamer serialises on the durable write.
- **`notifier`** — write non-durably (returns at once with an engine
  seqnum) and acknowledge via a single **durable-seq watcher**
  (`Db.Status().DurableSeq`). The streamer pipelines the next append
  while prior ones await durability. This is the "durability
  notifier" / s2-lite technique.

Single stream, one process — concurrent appends/s across storage
backends:

**flush = 10 ms** (fast local durability):

| conc | in-memory `sync` | `notifier` | file `sync` | `notifier` | MinIO `sync` | `notifier` |
| ---- | ---- | ---- | ---- | ---- | ---- | ---- |
| 128  | 5,706 | 11,366 | 5,669 | 11,301 | 5,740 | 11,160 |
| 512  | 5,666 | 45,370 | 5,683 | 45,072 | 5,700 | 43,371 |
| 1024 | 5,700 | 86,996 | 5,638 | 82,579 | 5,683 | 72,528 |

The three backends track each other: **at a 10 ms cadence, the flush
interval dominates, not disk or the loopback object-store PUT**
(in-memory ≈ real fsync ≈ MinIO). `sync` stays flat at about
`maxBurst/flush` (~5.7k); `notifier` scales with concurrency.

**flush = 50 ms on MinIO** is the S3/R2-like cadence you tune for on
real object storage (batch WAL PUTs to cut request cost). This is where
`notifier` earns its keep:

| conc | `sync` | `notifier` | speedup |
| ---- | ---- | ---- | ---- |
| 128  | 1,218 | 2,463  | 2×    |
| 512  | 1,219 | 9,952  | 8×    |
| 1024 | 1,212 | 19,913 | **16×** |

`sync` collapses to about `maxBurst/flush` = 1.2k/s regardless of load;
`notifier` holds about 20k/s. On real S3 (tens-of-ms PUT latency), the
gap widens further. This is the "usable vs not" line for an
object-storage deployment.

**Notes.** `notifier` does not change single-client sequential latency:
a client that waits for each acknowledgement pays one flush per append
in both modes. The win is *concurrent* throughput. In-flight appends are
bounded by client concurrency, since each blocked request holds one.

**Correctness.** The full suite passes in both modes under `-race`
(`YASDB_TEST_DURABILITY=notifier make test`). The Jepsen Maelstrom kafka
log-consistency checker (`test/maelstrom`) passes in both modes on the
in-memory store, **and against a MinIO-backed server in `notifier`
mode** — so async durability preserves log consistency over the
object-storage hop.

Object-store benchmarks are gated behind `YASDB_BENCH_STORE` (skipped by
default). To reproduce, run MinIO and:

```sh
export AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=… AWS_REGION=us-east-1 \
       AWS_ENDPOINT=http://127.0.0.1:9000 AWS_ALLOW_HTTP=true
YASDB_BENCH_STORE=s3://yasdb YASDB_BENCH_FLUSH=50ms \
  YASDB_TEST_DURABILITY=notifier make bench ARGS='-bench=BurstS3'
```

## Concurrency / mutex audit

This audit covers every access to shared mutable state (found via `git
grep` of the fields):

- **`streamer.meta`, `producers`, `writerSeq`** — mutated and read
  **only on the streamer goroutine** (validation, commit, and
  read-touch all run there). Readers never touch them directly; they
  use the immutable `contentType`/`isJSON`/`id` and the atomic
  `tail`/`closed`/`longPollReaders`/`sseReaders`/`waiters`. No lock is
  needed, so there is no race.
- **Resident-streamer registry** (`streamerRegistry`, `registry.go`) —
  sharded 64 ways by a hash of the stream path, with each shard holding
  its own cache-line-padded mutex and map. This was previously a
  single global `regMu`; a pprof mutex profile at 32768 resident
  streams under 400 concurrent VUs showed 100% of sampled contention
  through that one lock.
- **Spawn-vs-delete serialization** (`spawnLocks`, `registry.go`) —
  also sharded 64 ways by path hash, used by `getOrSpawn` and
  `removeStreamerLocked` (`expiry.go`) in place of the old global
  `metaMu`, for this specific race only. `metaMu` still serializes
  everything else (id allocation, create/fork idempotency, delete's
  ref-count cascade): `nextID` is a genuinely global monotonic counter
  and cannot be sharded the same way. But `getOrSpawn` never touches
  it, so this narrower fix did not need to shard `metaMu` fully. Found
  via the same technique: a pprof mutex profile at 32768 streams showed
  99.85% of sampled contention through `getOrSpawn`'s old `metaMu`
  lock. After sharding, `getOrSpawn`'s own share dropped to 19.95%,
  redistributed to genuinely different things (the committer pool's
  lock, SlateDB's own `Get` call), not a lingering bottleneck.
  `TestForkCascadeDeleteSpawnLockCollision` forces two paths to hash to
  the same shard and exercises the fork-cascade-delete path through it,
  since the lock is deliberately scoped tightly (just "retire streamer
  and commit", not the whole delete) to avoid a self-deadlock when a
  cascade touches a different path on the same goroutine.
- **Shared committer pool** (`sharedCommitter`, `commit.go`, notifier
  mode only — see
  [`docs/rfcs/0001-cross-stream-group-commit.md`](docs/rfcs/0001-cross-stream-group-commit.md)) —
  a `GOMAXPROCS`-sized pool of worker goroutines, hash-routed by stream
  id, each coalescing bursts from *whichever streams* land in its queue
  into one `WriteBatch`. This replaces one `CommitAsync` call per
  stream's own burst with one call shared across many streams, so
  throughput scales with total system load instead of per-stream
  concurrency. This is the fix for the streams-count throughput decline
  described below.
- **Lock ordering** — `st.mu → shard.mu` (`tryRetire`→`unregister`).
  `metaMu` and the two sharded locks never nest with `st.mu` the other
  way. **A real inversion was found and fixed** (pre-sharding):
  `Server.Close` held the registry lock while taking each `st.mu`, the
  reverse of `tryRetire`. `Close` now drains the registry, releases it,
  then stops each streamer, matching `removeStreamer`. Regression test:
  `TestConcurrentCloseNoDeadlock`.
- **Background work vs store teardown** — the sweeper and every
  background deletion run on a `WaitGroup` that `Close` joins before
  freeing the store, so a resumable range-delete scan can never hit an
  already-destroyed SlateDB handle. The committer pool's own
  `WaitGroup` is joined the same way, before the durability notifier
  shuts down.
- The whole suite passes under `-race` (including a 200-way concurrent
  append, the shutdown-vs-retire stress, and the forced spawn-lock
  collision above), in both durability modes and against both storage
  backends.

Note: `Server.Close()` frees the native store, so callers must stop
accepting requests first (drain in-flight handlers). `main.go` calls
`httpSrv.Shutdown()` before `srv.Close()`. Calling `Close` with FFI calls
in flight is a use-after-free at the binding level, independent of
Go-level locking.

## Stream-count scaling

Throughput per stream declined sharply as the number of
concurrently-active streams grew, even at fixed total request
concurrency. This was measured directly, same build, same VUs, clean
volume:

| Streams | Throughput | Drop vs. 1,024 |
| ------- | ---------- | --------------- |
| 1,024   | 16,144 records/s | — |
| 32,768  | ~9,400-9,561 records/s | ~42% |

Root cause: each stream's own goroutine only ever batches *its own*
queued appends (`maxBurst`). With fixed total concurrency spread across
more streams, each stream sees fewer concurrent writers, so its own
batches shrink toward size 1. This erodes the group-commit amortization
the whole design depends on. This is confirmed architectural, not
backend-specific: it reproduces on both SlateDB and pwal, and it was
independently reproduced in two external SlateDB-adjacent references
researched for
[RFC 0001](docs/rfcs/0001-cross-stream-group-commit.md). Both converge
on a small, fixed pool of committers shared across every stream, not one
committer per stream.

After the shared committer pool and spawn-lock sharding above, this was
measured with [`vegeta`](https://github.com/tsenart/vegeta) against the
real protocol path, full validation, tombstones, and live-reader
machinery included. (The load-tool switch to vegeta happened because a
per-VU JS interpreter was itself a bottleneck at this scale — see below.)

| Streams | Throughput | Drop vs. 1,024 |
| ------- | ---------- | --------------- |
| 1,024   | 17,140 records/s | — |
| 32,768  | ~24,300-28,200 records/s (peak 24-27k via the earlier JS-based load tool, 28,178 via vegeta at 800 workers) | **flat to positive** |

The stream-count cliff is gone. At 32,768 streams, CPU idle bottoms out
around 19% (81%+ busy): a genuine compute ceiling now, not an
architectural one.

**Load-tool caveat, found the hard way:** the earlier JS-based load
generator hit its own CPU ceiling (98%) producing a "raw append"
diagnostic workload (batching kept via the same committer pool, all
protocol machinery stripped — idempotency, ordering, tombstones, TTL,
live-reader wake/cache), while yasdb was still 44% idle. The client was
the bottleneck, not the server. Switching to `vegeta` (compiled Go, no
JS interpreter) on the identical workload took throughput from 16,627/s
to 26,770/s at the same worker count, and to 35,956/s at 800 workers
before yasdb's own latency-based saturation (not CPU) set in.
`deploy/loadtest/Dockerfile` now builds `vegeta` and the
`benchmark/loadshape` tool for exactly this reason. See
[`benchmark/README.md`](./benchmark/README.md) for the current
load-testing tools.

**How much is protocol machinery vs. raw batching?** At least about 33%
of throughput at 32,768 streams (11,095/s full protocol vs. 16,627/s raw
append, both client-bottlenecked at that sample, measured with the
earlier JS-based load tool) — likely more, since the raw number was
itself client-limited. The diagnostic code that produced this measurement
was deliberately disposable and has since been removed.
