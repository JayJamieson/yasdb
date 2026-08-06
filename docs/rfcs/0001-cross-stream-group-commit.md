# RFC 0001: Cross-stream group commit

`commit.go` (`sharedCommitter`) is referenced by this filename in several
doc comments, but the file never actually existed until now. This backfills
it, and records a since-completed investigation into simplifying the
design it documents.

## Current design

Group commit in yasdb happens at two independent layers:

1. **Per-stream** (`streamer.go`, `drainBurst`/`processBurst`, both
   durability modes). One stream's own goroutine folds any already-queued
   appends (up to `maxBurst`, 512) into one `pendingBurst`, validated in
   order against a single view (idempotent-producer checks, `Stream-Seq`
   monotonicity, TTL sliding). This has to be per-stream and ordered — it's
   where correctness lives — and is not what this RFC is about.
2. **Cross-stream** (`commit.go`, notifier durability only). `sharedCommitter`
   is a `runtime.GOMAXPROCS(0)`-sized pool of workers, each owning a
   channel. A stream is hash-routed to a fixed worker
   (`workerFor`/`fnv1aU64`), so its bursts always drain through the same
   worker's FIFO queue — needed because `durabilityNotifier` fires
   callbacks in the target sequence number's order, and two bursts from one
   stream must reach `CommitAsync` in the order they were produced. A
   worker folds whatever jobs (from however many different streams) are
   already queued when it wakes, up to `CommitterBatchMax` (512), into one
   shared `WriteBatch`/`CommitAsync` call.

This is the layer under review: a `committerWorker` struct (mutex, closed
flag, channel), a hash function, a non-blocking drain-with-`select`/`default`
loop, and coordinated N-worker shutdown — a lot of moving parts for what
the doc comments describe as "coalesce whatever's waiting."

## Investigation

### Research

**s2-lite** (`s2-streamstore/s2`, `lite/src/backend/streamer.rs`) is
yasdb's own cited precedent for the notifier technique
(`durability.go`). Reading its actual source: it does **no cross-stream
batching at all**. Each stream's task submits its own `WriteBatch`
immediately on every accepted append — no worker pool, no batching size or
timer. Backpressure is a single semaphore over aggregate in-flight bytes.
The "many concurrent submitters land in one physical flush" effect comes
entirely from SlateDB's own WAL flush-interval buffering, not from a
Go/Rust-side merge layer.

**bbolt's `DB.Batch()`** (`etcd-io/bbolt`) is the closest thing to a
battle-tested reference for what `sharedCommitter` is doing, and does it in
far less code: one shared `batch{db, timer, sync.Once, calls}` struct, one
mutex to append a caller's function to it, a size-*or*-timer trigger,
`sync.Once` for exactly-once execution, and a clean failure-isolation
retry (evict the one call that errored, requeue the rest solo). No worker
pool, no hash routing — there is only ever one physical transaction, so
ordering is free.

Both pieces of prior art suggested the same thing: collapse
`sharedCommitter` down to a single shared queue, on the reasoning that (a)
`BenchmarkAppendBurst1024Flush10ms` already showed one worker alone (1024
producers on one stream, which hashes to exactly one worker) scaling to
45,490 appends/s, so the folding-into-fewer-calls mechanism clearly doesn't
need multiple workers to pay off; and (b) a single global queue trivially
subsumes hash-routing's ordering guarantee (only one physical commit in
flight, ever, so no two streams' bursts can race each other at all).

### The prototype

`queueCommitter`: one `sync.Mutex`-guarded pending-jobs slice, a
`flushing` bool. `commit()` appends the job; if no flush is running, it
takes the whole slice and starts one. The flush loop calls `CommitAsync`
once, subscribes every job to `durabilityNotifier` under the returned seq
(unchanged from `sharedCommitter`), then re-checks the pending slice and
loops if more arrived while it was running — the same "stage while a flush
is in-flight, then loop" discipline already used elsewhere in this
package (`pwalShard.stage`/`runFlush`). No channels, no hash function, no
per-worker struct, no N-way shutdown coordination.

It passed the entire existing functional suite under `-race`, both storage
backends, notifier durability. Correctness was never in question — this
was a real, working alternative.

### Benchmark: it loses

Every existing benchmark before this RFC used **one stream**, which
hash-routes to exactly one `sharedCommitter` worker regardless of pool
size — so none of them exercised the worker pool's actual reason for
existing: folding bursts *across different concurrently-hot streams*.
`BenchmarkAppendBurstManyStreams*` (`bench_test.go`) fills that gap: N
streams, each with its own producer(s), notifier durability.
`producersPerStream=1` is the purest signal — with only one producer per
stream, a stream's own `drainBurst` can never coalesce anything (never
more than one queued append per stream), so any throughput above the
serial baseline can only come from cross-stream folding.

Same machine, same session, `-benchtime=2s`, repeated:

| Benchmark | `sharedCommitter` (current) | `queueCommitter` (prototype) | Delta |
| --- | --- | --- | --- |
| 1024 streams × 1 producer | 62,764 – 64,072/s | 50,931 – 55,287/s | **−13% to −20%** |
| 256 streams × 4 producers | 74,349 – 75,321/s | 64,430 – 64,459/s | **−14% to −16%** |
| 64 streams × 1 producer | 5,721/s | 5,724/s | ~0% |
| 1 stream × 128 producers (existing bench) | 6,146/s | 6,161/s | ~0% |
| 1 stream × 1024 producers (existing bench) | 45,490/s | 45,096/s | ~0% |

Single-stream numbers are identical, as expected — both designs behave the
same when only one worker is ever involved. The multi-stream numbers are
where they diverge, consistently, across repeated runs: the simplified
single-queue design gives up **13–20% aggregate throughput** the moment
more than a handful of different streams are simultaneously hot, because
it caps physical `CommitAsync` calls at one in flight at a time,
process-wide, where `sharedCommitter` can have up to `GOMAXPROCS` in
flight concurrently. That parallelism — not the folding itself — is what
the multi-stream numbers show being worth its complexity.

This machine has 2 vCPUs, so `GOMAXPROCS(0)` is 2 here: the measured gap
is with only **two** concurrent workers. On a larger production box (more
cores → more workers), the gap this trades away would plausibly be larger,
not smaller.

## Revisit: the sandbox result did not hold on real hardware

The first version of this RFC stopped here, on a "keep it, the numbers say
so" verdict — reasonably, given what was measured. That measurement was
rightly not trusted as final: it came from a 2-vCPU sandbox with an
**in-memory** SlateDB store (`memory:///`), nothing like production's
4-vCPU box with a real volume-backed store and a real network hop for
every commit. So the comparison was re-run for real, against the actual
`yasdb` Fly app (`performance-4x`, `syd`), using an in-region load
generator (`deploy/loadtest`, over 6PN so the measurement isn't WAN-latency
noise — see that directory's README) and the same `-admin-bulk-provision`
setup the `benchmark/` scripts use: 1024 streams, 300 concurrent vegeta
workers, 20s attacks, cycling through the stream pool round-robin (the
same "many concurrently hot streams" shape `BenchmarkAppendBurstManyStreams`
targets, just over real HTTP instead of in-process Go calls).

`queueCommitter` was re-added (same code, same `YASDB_COMMITTER_MODEL=queue`
toggle used for the sandbox A/B) and deployed to the real machine. Runs
were interleaved by flipping the env var and restarting the machine
between samples, no other change:

| Run | `sharedCommitter` (current) | `queueCommitter` (prototype) |
| --- | --- | --- |
| 1 | 25,304 req/s | 24,915 req/s |
| 2 | 24,503 req/s | 26,047 req/s |
| 3 | 24,036 req/s | 23,177 req/s |
| range | 24,036 – 25,304 | 23,177 – 26,047 |
| 100% success, both sides, all runs | p50 ≈ 10.7–11.4ms, p99 ≈ 20.9–22.1ms | (same shape both sides) |

**The sandbox's 13–20% gap does not reproduce.** The two ranges overlap
almost entirely, with no directional trend across three interleaved
samples each. On the hardware and storage backend that actually matters,
`sharedCommitter`'s worker-pool parallelism is not earning a measurable
throughput advantage over a single shared queue.

The most likely reason the sandbox result was misleading, not just noisy:
against an in-memory store on 2 vCPUs, the bottleneck the worker pool
relieves (contention on a single Go-side commit path) is a real, visible
fraction of total cost. Against a real volume-backed store, the physical
write itself (SlateDB's own WAL flush / durability machinery, per
`BENCHMARKS.md`'s CPU-profile finding that the append path is dominated by
`libslatedb_uniffi`/`cgocall`, not application code) dominates enough that
however many Go-side workers are feeding it stops mattering — one shared
queue keeps the engine just as busy as two competing workers did.

## Decision: shipped

**Adopted.** Real-hardware measurement showed no throughput cost, and the
single-shared-queue design deletes `committerWorker`, `workerFor`/
`fnv1aU64` hash routing, and N-way worker-pool shutdown coordination — the
actual "too much to hold in my head" complexity this RFC was opened to
address. The queue design now lives under the existing `sharedCommitter`
name (kept — it's still accurate: a committer shared across streams, just
a queue instead of a pool now) in `commit.go`; there is no separate type
or env-var toggle left behind. `Config.CommitterWorkers` (a worker-pool
sizing knob with nothing left to size) and its `-committer-workers` CLI
flag were removed along with it. `Config.CommitterBatchMax` stays — it now
bounds one physical commit's size directly, the same safety property the
old per-worker `batchMax` had.

Full functional suite passes under `-race`, both storage backends, both
durability modes, after the change. Deployed to production
(`yasdb.fly.dev`) and confirmed healthy.

`maxBurst`/`drainBurst` (per-stream) is unrelated to this investigation and
untouched — it's necessary for validation ordering, not a throughput lever
this RFC was evaluating.

## Lesson for next time

The sandbox benchmark was internally consistent, reproducible, and still
wrong for the decision it was informing, because it exercised a materially
different bottleneck (Go-side scheduling contention on an in-memory store)
than production has (engine-side durability I/O on a real volume). A
benchmark's reproducibility is not evidence it's measuring the right
thing — for a durability-critical path, a real-hardware/real-backend check
before finalizing a decision anyone will act on is worth the Fly.io
round-trip it costs, not an optional follow-up.

## Addendum: mutex profiling after the change landed

A follow-up question after the decision shipped: does the new single-mutex
design actually show up as *less* mutex contention in a real profile, and
does `streamer.go`'s per-stream `mu` still exist? Answered with
`go test -mutexprofile` against `BenchmarkAppendBurstManyStreams*`
(`-mutexprofilefraction=1`, every event, not sampled), not by inference.

### A benchmark bug surfaced first

The first profile run was almost useless: 90.84% of all sampled mutex
delay traced through `getOrSpawn`. Surprising, since
`BenchmarkAppendBurstManyStreams` pre-creates every stream with
`mustCreate` before `b.ResetTimer()` — there should be nothing left to
spawn once timing starts.

Turned out `mustCreate` is a `PUT`, and `createStream` (`handlers.go`)
only persists stream *metadata* — it does not spawn a resident `streamer`
goroutine. Spawning is lazy, via `getOrSpawn`, on first touch
(`server.go`). So every one of the benchmark's `numStreams` streams
cold-spawned **simultaneously**, all piled onto `b.ResetTimer()`, clustering
a one-time registry-spawn-lock storm into the very start of the timed
region. `getOrSpawn.gowrap2`'s and `submitAppend`'s cumulative delay in
that first profile — over 20 of the 22.5 seconds total — was almost
entirely this startup spike, not steady-state committer or registry
contention. A benchmark can be internally consistent and still be
measuring its own cold-start instead of the thing it's named for; this is
the same lesson as the sandbox-vs-real-hardware finding above, one level
down.

Fixed in `bench_test.go`: one warm-up `appendOnce` per stream before
`b.ResetTimer()`, so every streamer is already resident when timing
starts. This doesn't change this RFC's throughput conclusion — the
cold-spawn cost lives in the registry, identical for both committer
designs, so it couldn't have biased the A/B's *direction* — but it made
every profile after it dramatically cleaner, and it's a permanent fix:
this benchmark now measures what its name says it measures.

### Steady-state result, `1024 streams × 1 producer`

No stream here ever has two producers, so this isolates the committer
from any per-stream contention:

| Lock | Share of mutex delay |
| --- | --- |
| `sharedCommitter`'s single queue mutex (`commit` + `runFlush`'s repeated re-lock to check `pending`) | **53.3%** |
| registry shard lock + `streamer.mu` combined | 1.7% |

### Steady-state result, `256 streams × 4 producers`

Now streams genuinely have contention — 4 producers sharing one stream's
lock:

| Lock | Share of mutex delay |
| --- | --- |
| **`streamer.mu` (per-stream)** | **60.4%** |
| `sharedCommitter`'s queue mutex | 36.7% |

### Does the per-stream mutex still exist, and why does it exist at all?

Yes — `streamer.mu` (`streamer.go`) is untouched by this RFC. `go tool
pprof -list` on `submitAppend` (`server.go`) pins the contention to one
line:

```go
st.mu.Lock()
if st.dead {
    st.mu.Unlock()
    st = nil
    continue
}
req.resp = make(chan appendResp, 1)
st.reqs <- req
st.mu.Unlock()   // <- 4.24s of 7.13s total delay, the 256x4 profile
```

The natural question: `st.reqs` is a channel, and channel send/receive is
already safe for concurrent use without external locking — so what is
`st.mu` actually protecting, if not the send itself?

**Not the mechanics of the send. The *liveness* of the receiver on the
other end.** A channel's built-in safety guarantees a concurrent send
won't corrupt memory or race with another send. It guarantees nothing
about whether anything is still receiving from that channel, or ever will
be again. `st.reqs` is buffered (`maxBurst`, 512), so a send succeeds
immediately regardless of whether the streamer's goroutine is still
running its `select` loop in `run()` — including after that goroutine has
already exited, via either retirement path:

- **Idle timeout** (`tryRetire`, `streamer.go`): checks `len(s.reqs) ==
  0`, unregisters from the registry, sets `dead = true`, and returns —
  `run()`'s `for` loop ends. No more `select` iterations, ever, for this
  streamer.
- **Forced retirement** (`removeStreamer`, delete): sets `dead = true` and
  closes `stop`. `run()`'s `stop` case does one non-blocking sweep of
  `s.reqs` to 404 anything already queued, then returns — same ending,
  just with a cleanup pass first.

Without `st.mu` making "check `dead`" and "send to `reqs`" one atomic
step, there's a real window: `submitAppend` reads `dead == false`, the
streamer retires in the gap before the send, and the request lands in a
channel nobody will ever read again. Not a crash — a silent, permanent
hang: `submitAppend` blocks forever on `<-req.resp`, because nothing will
ever produce that response. `st.mu` closes exactly that window, by making
the dead-check and the enqueue indivisible relative to whichever mutex-
guarded step actually flips `dead` to `true`.

This is not a one-off pattern in this codebase — it's the same shape as
`durabilityNotifier.subscribe`'s `closed` check (`durability.go`) and
`sharedCommitter.commit`'s own `closed` check (`commit.go`, this RFC's
design): "check not-closed, then hand off work" has to be atomic with
"mark closed, then stop servicing," anywhere a goroutine can stop
listening while another goroutine might still be about to send it
something. A channel's thread-safety covers the send; it says nothing
about whether the send was heard, and *that's* what the mutex is for.

### What this means for the RFC's decision

Nothing changes it. `sharedCommitter`'s queue mutex genuinely is now the
single most-contended lock in the system when many streams are hot
(53.3%, no per-stream noise) — that is the literal mechanism behind this
RFC's own tradeoff framing ("less bookkeeping" traded for "at most one
physical commit in flight": fewer, more-contended locks is what that
looks like under a profiler). What the profile does *not* show, and what
the real Fly.io A/B already demonstrated, is that this contention costs
throughput: the lock is held for microseconds — append to a slice, check
a flag — so "most-contended" and "slow" are not the same claim here. On
real hardware, SlateDB's own engine-side durability I/O is the actual
ceiling, well below whatever this lock could cost even fully contended.
Contention and cost are different measurements; this addendum is the
former, RFC 0001's main body is the latter, and the latter is what the
decision was made on.
