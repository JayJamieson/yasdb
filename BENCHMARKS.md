# Performance & concurrency notes

Run: `make bench` (adds `-benchmem`). Machine: 2 vCPU AMD EPYC, in-memory object
store unless noted, 11-byte records.

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

**1. Append latency is the SlateDB WAL flush interval.** An `AwaitDurable` write
blocks until the next flush, so serial durable-append latency ≈ `flush_interval`
(100 ms default → 9.9/s; 10 ms → 89/s; 1 ms → 455/s). This is now configurable
with the `-flush` flag (default **10 ms**) and `OpenStore(..., flushInterval)`.
Lower = lower latency but more frequent object-store writes (matters for S3
cost), so it's a knob, not a free win.

**2. Group commit removes the throughput penalty.** With 128 concurrent producers
on one stream, the streamer folds queued appends into a single durable batch (up
to `maxBurst`=64), giving **611 appends/s at a 100 ms flush** — a **~62×**
speed-up over the 9.9/s serial rate at the same flush, and **5,668/s at 10 ms**.
This is the spec's pipelining benefit, working: throughput is no longer divided
by the flush interval. The append path is **not CPU-bound** — the CPU profile
shows only ~13% utilization during the burst, almost all of it in `libc` /
`libslatedb_uniffi` / `runtime.cgocall` (the durability machinery); yasdb's own
`planAppend`/`processBurst` code is a rounding error.

**3. Durability backend is not the limit at these rates.** The on-disk file store
(real fsync) matched the in-memory store (**5,686 vs 5,668/s**) — the WAL flush
*cadence* and coalescing dominate, not disk I/O.

**4. The read bottleneck is the SlateDB binding's per-record FFI cost.** A
100-record catch-up read allocates **1,848 objects (~18/record)**, and the alloc
profile attributes **87 %** of them to `uniffiRustCallAsync` and the RustBuffer
deserialisation (`FfiConverterOptionalKeyValue.Read`, `encoding/binary.Read`,
`bytes.NewReader`) inside `DbIterator.Next()`. Every record crossed the CGo
boundary as an async UniFFI call. This is exactly SPEC.md §13.3's concern — the
fix is a **batched scan entry point in the binding**, which is upstream; yasdb's
own read code (offset assembly, JSON wrapping) is negligible by comparison.

### Tunables

- `-flush <dur>` — durability latency floor (default 10 ms).
- `-durability sync|notifier` — see below.
- `maxBurst` (const, 64) — max appends folded per group commit; raising it
  increases coalescing under very high concurrency at the cost of larger batches.

## Connected-reader capacity (read fan-out)

The read numbers above measure **active** reads — clients pulling bytes as fast as
they can, which is CPU-bound (§4: per-record FFI dominates, ~2 cores saturate
immediately). A separate question is how many **connected but idle** readers a
single process can hold — long-poll / SSE clients parked waiting for the next
record. That ceiling is set by **memory**, not CPU, ports, threads, or fds.

Measured by opening N concurrent SSE readers (`?offset=now&live=sse`) against one
in-memory server (`-flush 5ms`) and holding them open (load generator in
`readbench -hold`, multi-source-IP dialing to get past the client's ~28k
single-IP ephemeral-port limit):

| connected SSE readers | server RSS | server threads | failures |
| --- | --- | --- | --- |
| 0 (baseline) | ~16 MB | — | — |
| 5,000 | 163 MB | 13 | 0 |
| 15,000 | 453 MB | 16 | 0 |

Clean linear fit: **~29 KB of server RSS per connected reader + ~18 MB base.**

**What does *not* cap it:**

- **Threads.** SSE/long-poll waiters park on a Go **channel** (`waiterChan`), not
  an OS thread, so the process held 15k+ readers at only **16 OS threads** — the
  Go runtime's ~10k-thread crash ceiling is never approached. (This is the payoff
  of not blocking a goroutine-per-connection on a cgo call.)
- **File descriptors.** `ulimit -n` is 524,288 here — orders of magnitude above
  the RAM ceiling.
- **Ephemeral ports.** ~28k per source IP is a *client-side* limit (worked around
  with multiple source IPs); the server binds one listening socket.

**Where it fails over: RAM.** At ~29 KB/reader the ceiling is memory exhaustion.
On a box with ~2.5 GB usable to the server alone this extrapolates to **≈ 80k
connected readers** before OOM — no other resource intervenes first. That number
is *extrapolated* from the linear fit, not measured to the wall: measuring the
true tip-over point requires the load generator on a **separate** machine, because
a co-located client costs its own per-connection memory and the two compete for
the same RAM (on the 3.8 GB test box the *joint* client+server ceiling is roughly
half the server-only figure, and driving toward it OOMs the whole VM). Rule of
thumb for capacity planning: **budget ~30 KB of server RAM per concurrent
connected reader**; everything else scales out of the way.

**Caveat — this RAM ceiling is for *idle* readers.** The numbers above were
measured with readers parked on a stream that was **not being written**. The
moment a stream is *actively written*, each commit wakes every parked reader
(fan-out), and for a hot stream that CPU cost — not RAM — becomes the first wall,
at a reader count well below the RAM extrapolation. See the next section.

## Live fan-out: the wake cost and coalescing (`-live-coalesce`)

A parked long-poll/SSE reader is cheap until the stream is written. On each commit
the streamer closes a shared broadcast channel, waking **every** parked reader,
and each woken reader re-scans the store for the new records (per-record cgo/FFI,
§4). So a commit that appends one record to a stream with N live readers costs
O(N) wakeups **and** O(N) store scans — a thundering herd. Under high
commit-rate × many readers this outruns the cores: with N readers and ~`c` CPU
per woken reader, the live path stays stable only while
`commit_rate × N × c < cores`, and above that the herd never drains between
commits and throughput collapses (a metastable cliff, not a transient spike). On
this 2-core box that threshold is a low commit rate once N reaches the
tens of thousands — which is why the *written*-stream reader ceiling is far below
the idle RAM ceiling.

**`-live-coalesce <dur>`** (default `0` = wake on every commit, the original
behavior) bounds the wake rate with leading-edge debounce: a commit after a quiet
gap ≥ window wakes readers immediately (no added latency for low-rate streams),
and commits inside the window fold into a single trailing wake. It never drops or
reorders data — readers always re-read to the current tail, so one wake after a
burst delivers every accumulated record in order. It trades up to `window` of
extra delivery latency on hot streams for a bounded wake rate.

Measured with a fixed pool of live SSE readers on one written stream, a single
writer landing commits, in-memory store (`BenchmarkLiveFanout`, non-`-race`):

| readers | mode | ns/commit (fan-out) | broadcasts / commit | deliver/s |
| --- | --- | --- | --- | --- |
| 100 | `0` (v1) | 3,121,156 | 1.00 | 32,039 |
| 100 | `10ms`   | 2,454,152 | 0.235 | **40,747** |
| 500 | `0` (v1) | 15,043,890 | 1.00 | 33,236 |
| 500 | `10ms`   | 14,184,225 | 0.87 | 35,250 |
| 500 | `50ms`   | 5,354,542 | 0.107 | **93,379** |

Two takeaways:

1. **Coalescing works and compounds with fan-out.** At 500 readers a 50ms window
   folds ~9 commits per wake: **9.4× fewer broadcasts (1.00→0.11 per commit),
   2.8× lower per-commit latency, 2.8× more delivery throughput.** It also frees
   the *writer* — under `-race` contention the writer's own append phase dropped
   from 3.9 s to 1.2 s once it stopped competing with the herd.
2. **The window must exceed the commit interval.** At 500 readers the per-commit
   fan-out already costs ~15 ms, so commits naturally space out past a 10 ms
   window and leading-edge fires on nearly every one (0.87/commit — little
   benefit). Widening to 50 ms recovers it. Tune the window to the fan-out /
   acceptable latency, not to a fixed number.

Coalescing bounds the wake *rate*, but each wake still made every reader re-scan
the store per-record over cgo. The **in-memory record cache** (the "broadcast
hub") removes that: on commit the streamer caches the bytes it just wrote, and a
caught-up reader assembles its next chunk from RAM instead of scanning the store —
a commit becomes one in-memory fan-out instead of N cgo re-scans. It's on by
default (non-fork streams with live readers); `-no-live-cache` disables it.
Correctness is identical — `tryRead` serves only the common caught-up case (whole
records, fully in the retained window, under the read cap) and returns bytes
byte-for-byte equal to `readRange`; everything else (forks, mid-record byte
offset, behind the window, over the cap) falls back to the store scan.

Same fan-out, coalesce window fixed at 10ms, cache off vs on
(`BenchmarkLiveFanoutHub`):

| readers | cache | ns/commit | cache-hits/commit | deliver/s |
| --- | --- | --- | --- | --- |
| 100 | off | 2,391,825 | 0 | 41,809 |
| 100 | on  | 2,204,446 | 21.5 | 45,363 |
| 500 | off | 14,122,803 | 0 | 35,404 |
| 500 | on  | 2,786,231 | 131 | **179,454** |

At 500 readers the cache is **5.1× faster (14.1→2.8 ms/commit) and 5.1× more
delivery throughput (35k→179k/s)**, with ~100% of live reads served from RAM. It
also *compounds* with coalescing: cheaper reads let commits land faster, so more
fold per window (wakeups dropped 521→158 at 500 readers). Per-hot-stream memory is
bounded and small — the retained window defaults to **512 records / 256 KiB**
(`Config.LiveCacheMaxRecords` / `LiveCacheMaxBytes`, `-live-cache-bytes`), which a
caught-up reader never outruns (shrinking it 8× from an earlier 2 MiB left the hit
rate and throughput unchanged). It's **per stream, not per reader** — one cache
serves all of a stream's readers — and payloads are referenced, not copied (the
commit's `marshalRecord` buffer is already a private write-once copy). Write-only
streams, streams with no live readers, and forks keep no cache at all.

**Still on the store scan:** catch-up (`?offset=` one-shot) reads, forks,
mid-record partial reads, and readers behind the retained window — none of which
are the live fan-out hot path. Correctness coverage: `TestLiveCacheMatchesStore`
(differential — cache output == `readRange` byte-for-byte across every offset, both
binary and JSON), `FuzzLiveCacheMatchesStore` (property version — the fuzzer picks
content type / cap / record sizes, `tryRead` == `readRange` where the cache serves;
it found a real cap-boundary bug — a trailing zero-length record at `total == limit`
— now fixed by serving only strictly under the limit), `TestLiveFanoutCompare`
(byte-verified order + deterministic wake bound + no-regression), and
`TestReadHeavyStress` (60-reader byte-level check through both the coalesced wake
path and the cache), all under `-race` in both durability modes, plus
`make maelstrom READ=long-poll`.

### Read-path allocation profile (concurrent writers)

Profiling the fan-out (200 writers group-committing into one stream that 500
readers tail) put the CPU where it should be — the read path is **socket-I/O
bound, not cgo-bound** (`runtime.cgocall` 35%→1% vs cache off) — but showed SSE
**serialization** dominating server allocation, and the hot spot *shifts with
batch size*: control frame under many small commits, data frame under big
batches. Four fixes made the SSE read-serving path essentially allocation-free
(byte output unchanged; guarded by the byte-identity tests):

1. hoist `[]byte("literal")` framing constants to package vars;
2. `writeSSEData` fast-path — write the payload bytes verbatim when it has no
   CR/LF (the common case), skipping `string`/`Split`/`[]byte(line)`;
3. hand-roll `Offset.String` (no `fmt.Sprintf`);
4. hand-roll the control frame (no `json.Marshal`) — safe because every value is
   escape-free (offset digits/`_`, decimal cursor; pinned by
   `TestSSEControlFrameMatchesJSON`);
   plus the SSE handler reuses a per-connection body + control scratch buffer.

200-writer heap, before → after: `writeSSEData` **398 MB → <2 MB**, `tryRead`
body **196 MB → 6.5 MB**, control-frame `json.Marshal`/`fmt` **~30 MB → ~0**; total
allocations **1043 MB → 409 MB**, the remainder being the co-located load client
and the inherent write-durability path (`marshalRecord` + FFI commit). Throughput
rose (less GC). A multi-stream variant (writers × streams × readers) confirmed the
per-stream cache stays tiny in practice — ~**5 KB/stream** (far under the 256 KiB
cap, since a caught-up reader's window holds only a few commits) — so cache memory
scales at a few KB per hot stream (50 hot streams ≈ 195 KB total).

## Durability modes (`-durability`)

Both modes return `Stream-Next-Offset` only once the data is durable. They differ
in whether the append **blocks** on durability:

- **`sync`** (default) — each group commit blocks on `AwaitDurable`. Simple; a
  stream's throughput is capped at `maxBurst / flush` because the streamer
  serialises on the durable write.
- **`notifier`** — write non-durably (returns at once with an engine seqnum) and
  ack via a single **durable-seq watcher** (`Db.Status().DurableSeq`); the
  streamer pipelines the next append while prior ones await durability. This is
  the SPEC §6 "durability notifier" / s2-lite technique.

Single stream, one process — concurrent appends/s across storage backends:

**flush = 10 ms** (fast local durability):

| conc | in-memory `sync` | `notifier` | file `sync` | `notifier` | MinIO `sync` | `notifier` |
| ---- | ---- | ---- | ---- | ---- | ---- | ---- |
| 128  | 5,706 | 11,366 | 5,669 | 11,301 | 5,740 | 11,160 |
| 512  | 5,666 | 45,370 | 5,683 | 45,072 | 5,700 | 43,371 |
| 1024 | 5,700 | 86,996 | 5,638 | 82,579 | 5,683 | 72,528 |

The three backends track each other: **at a 10 ms cadence the flush interval
dominates, not disk or the loopback object-store PUT** (in-memory ≈ real fsync ≈
MinIO). `sync` is flat at ~`maxBurst/flush` (~5.7k); `notifier` scales with
concurrency.

**flush = 50 ms on MinIO** — the S3/R2-like cadence you tune for on real object
storage (batch WAL PUTs to cut request cost). This is where `notifier` earns its
keep:

| conc | `sync` | `notifier` | speedup |
| ---- | ---- | ---- | ---- |
| 128  | 1,218 | 2,463  | 2×    |
| 512  | 1,219 | 9,952  | 8×    |
| 1024 | 1,212 | 19,913 | **16×** |

`sync` collapses to ~`maxBurst/flush` = 1.2k/s regardless of load; `notifier`
holds ~20k/s. On real S3 (tens-of-ms PUT latency) the gap widens further — this
is the "usable vs not" line for an object-storage deployment.

**Notes.** `notifier` does not change single-client sequential latency — a client
that waits for each ack is one flush per append in both modes; the win is
*concurrent* throughput. In-flight appends are bounded by client concurrency
(each blocked request holds one).

**Correctness.** The full suite passes in both modes under `-race`
(`YASDB_TEST_DURABILITY=notifier make test`), and the Jepsen Maelstrom kafka
log-consistency checker (`test/maelstrom`) passes in both modes on the in-memory
store **and against a MinIO-backed server in `notifier` mode** — so async
durability preserves log consistency over the object-storage hop.

Object-store benchmarks are gated behind `YASDB_BENCH_STORE` (skipped by default);
to reproduce, run MinIO and:

```sh
export AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=… AWS_REGION=us-east-1 \
       AWS_ENDPOINT=http://127.0.0.1:9000 AWS_ALLOW_HTTP=true
YASDB_BENCH_STORE=s3://yasdb YASDB_BENCH_FLUSH=50ms \
  YASDB_TEST_DURABILITY=notifier make bench ARGS='-bench=BurstS3'
```

## Concurrency / mutex audit

Audited every access to shared mutable state (`git grep` of the fields):

- **`streamer.meta`, `producers`, `writerSeq`** — mutated and read **only on the
  streamer goroutine** (validation + commit + read-touch all run there). Readers
  never touch them; they use the immutable `contentType`/`isJSON`/`id` and the
  atomic `tail`/`closed`/`readers`/`waiters`. No lock needed, no race.
- **`Server.streamers` map** — always under `regMu`. **`Server.nextID`** — always
  under `metaMu` (creation). Verified all call sites.
- **Lock ordering** — `metaMu → regMu` (spawn/create/delete), `st.mu → regMu`
  (`tryRetire`→`unregister`). **Found and fixed a real inversion:** `Server.Close`
  held `regMu` while taking each `st.mu`, the reverse of `tryRetire`, so a
  shutdown racing with dormancy retirement could deadlock. `Close` now snapshots
  the registry under `regMu`, releases it, then stops each streamer — matching
  `removeStreamer`. Regression test: `TestConcurrentCloseNoDeadlock`.
- **Background work vs store teardown** — the sweeper and every background
  deletion run on a `WaitGroup` that `Close` joins before freeing the store, so a
  resumable range-delete scan can never hit an already-destroyed SlateDB handle.
- The whole suite passes under `-race` (including a 200-way concurrent append and
  the shutdown-vs-retire stress) in both durability modes and against both storage
  backends.

Note: `Server.Close()` frees the native store, so callers must stop accepting
requests first (drain in-flight handlers) — `main.go` does `httpSrv.Shutdown()`
before `srv.Close()`. Calling `Close` with FFI calls in flight is a use-after-free
at the binding level, independent of Go-level locking.
