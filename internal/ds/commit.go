package ds

import "sync"

// committer lands an assembled append burst durably and arranges the
// reader-visible publish and the client replies. Both modes return
// Stream-Next-Offset only once durable; they differ only in whether the
// append blocks on durability.
type committer interface {
	commit(st *streamer, c commitBurst)
	close() error
}

// commitBurst is the finalised pending view of one group commit: the
// assembled ops plus the per-request responses to send once the outcome is
// known.
type commitBurst struct {
	pend        *pendingBurst
	ops         []Op
	batch       []appendReq
	responses   []appendResp
	accepted    []bool
	metaChanged bool
	seg         cacheSeg
}

func newCommitter(store Storage, cfg Config) committer {
	if cfg.Durability == "notifier" {
		return newSharedCommitter(store, cfg)
	}
	return syncCommitter{store: store}
}

// syncCommitter blocks each group commit on a durable write. A stream's
// throughput is capped at maxBurst/flush because the streamer serialises on
// the durable write.
type syncCommitter struct{ store Storage }

func (sc syncCommitter) commit(st *streamer, c commitBurst) {
	if err := sc.store.Commit(c.ops, true); err != nil {
		failAccepted(c.batch, c.responses, c.accepted, err)
		replyAll(c.batch, c.responses)
		return
	}
	st.applyWriter(c.pend, c.metaChanged)
	st.applyReader(c.pend.tail, c.metaChanged && c.pend.closed, c.seg)
	replyAll(c.batch, c.responses)
}

func (syncCommitter) close() error { return nil }

// defaultCommitterBatchMax bounds how many different streams' bursts one
// physical CommitAsync call folds together — distinct from maxBurst, which
// bounds one stream's own queued requests. It exists to cap one commit's
// size (memory, and latency for whoever's waiting on it) under extreme
// fan-out, not for throughput: see RFC 0001 for why a single shared queue
// with this cap outperforms nothing on this dimension.
const defaultCommitterBatchMax = 512

// commitJob is one streamer's already-planned burst, handed to the shared
// committer. applyWriter has already run by the time a job is queued, so
// the committer only ever does I/O — no streamer-owned state is touched
// off the streamer's own goroutine.
type commitJob struct {
	st           *streamer
	c            commitBurst
	targetTail   uint64
	targetClosed bool
}

func failJob(j commitJob, err error) {
	failAccepted(j.c.batch, j.c.responses, j.c.accepted, err)
	replyAll(j.c.batch, j.c.responses)
	j.st.pending.Add(-1)
}

// sharedCommitter is the notifier-durability committer (RFC 0001): a
// single shared commit queue, not a worker pool. Every commit()'d job
// (from however many different streams) lands in one mutex-guarded
// pending slice; whichever caller finds no flush already running takes up
// to batchMax of it and runs the physical CommitAsync, looping onto
// whatever staged while that call was in flight (the same "stage while a
// flush is in progress, then loop" discipline pwalShard.stage/runFlush
// uses elsewhere in this package). Every stream gets one strict
// process-wide commit order for free this way — there is only ever one
// physical commit in flight, so two bursts can never reach CommitAsync out
// of order regardless of which streams they came from, and no hash-routing
// is needed to prevent it.
//
// An earlier design used a GOMAXPROCS-sized worker pool with streams
// hash-routed to a fixed worker, on the theory that spreading physical
// CommitAsync calls across workers would matter for cross-stream
// throughput. RFC 0001 measured both, in a sandbox and then for real
// against production Fly.io hardware: the worker pool showed a real,
// reproducible ~15% edge in an in-memory-store sandbox, and no measurable
// edge at all against a real volume-backed store on real hardware — the
// engine's own durability I/O dominates enough there that how many Go-side
// callers feed it stops mattering. This design won on real hardware and
// deleted the worker pool, the hash function, and the N-way shutdown
// coordination that came with it.
type sharedCommitter struct {
	store    Storage
	notifier *durabilityNotifier
	batchMax int
	wg       sync.WaitGroup

	mu       sync.Mutex
	pending  []commitJob
	flushing bool
	closed   bool
}

func newSharedCommitter(store Storage, cfg Config) *sharedCommitter {
	batchMax := cfg.CommitterBatchMax
	if batchMax <= 0 {
		batchMax = defaultCommitterBatchMax
	}
	return &sharedCommitter{
		store:    store,
		notifier: newDurabilityNotifier(store, cfg.NotifierPollInterval),
		batchMax: batchMax,
	}
}

// commit does the Go-side bookkeeping synchronously on the streamer's own
// goroutine. applyWriter must run before this returns, since the next
// burst's baseSeq is read from st.writeTail immediately after. Only the
// actual CommitAsync call and durability tracking are expensive I/O, so
// only that part runs on the flush goroutine.
//
// applyWriter runs even on the path that ends in failJob (the committer is
// already closed, i.e. Server.Close). This is harmless: this streamer's
// state is about to be discarded, and applyReader (the reader-visible
// publish) never runs for a failed job.
func (sc *sharedCommitter) commit(st *streamer, c commitBurst) {
	st.applyWriter(c.pend, c.metaChanged)

	job := commitJob{
		st:           st,
		c:            c,
		targetTail:   c.pend.tail,
		targetClosed: c.metaChanged && c.pend.closed,
	}
	// pending gates retirement: a streamer must not retire while a
	// durability callback is outstanding, or a respawn could read a stale
	// durable tail.
	st.pending.Add(1)

	sc.mu.Lock()
	if sc.closed {
		sc.mu.Unlock()
		failJob(job, errStoreClosed)
		return
	}
	sc.pending = append(sc.pending, job)
	if sc.flushing {
		sc.mu.Unlock()
		return
	}
	sc.flushing = true
	batch := sc.takeBatchLocked()
	sc.wg.Add(1)
	sc.mu.Unlock()
	go sc.runFlush(batch)
}

// takeBatchLocked removes up to batchMax jobs from the front of sc.pending
// and returns them, leaving any excess still queued for the next loop
// iteration in runFlush. Must be called with sc.mu held.
func (sc *sharedCommitter) takeBatchLocked() []commitJob {
	if len(sc.pending) <= sc.batchMax {
		batch := sc.pending
		sc.pending = nil
		return batch
	}
	batch := append([]commitJob(nil), sc.pending[:sc.batchMax]...)
	sc.pending = sc.pending[sc.batchMax:]
	return batch
}

// runFlush folds jobs (from however many different streams contributed
// them) into one CommitAsync call, subscribes each to the durability
// notifier under the returned seq — subscribe is called per job in drain
// order, and equal-target subscribers fire in that same order (subscribe's
// tie-breaking), so two jobs for the same stream still resolve in
// submission order — then loops onto whatever staged while that call was
// in flight. Exits (and releases wg) once the queue is empty.
func (sc *sharedCommitter) runFlush(jobs []commitJob) {
	defer sc.wg.Done()
	for {
		totalOps := 0
		for _, j := range jobs {
			totalOps += len(j.c.ops)
		}
		ops := make([]Op, 0, totalOps)
		for _, j := range jobs {
			ops = append(ops, j.c.ops...)
		}
		seq, err := sc.store.CommitAsync(ops)
		if err != nil {
			for _, j := range jobs {
				failJob(j, err)
			}
		} else {
			for _, j := range jobs {
				j := j
				sc.notifier.subscribe(seq, func(cerr error) {
					if cerr == nil {
						j.st.applyReader(j.targetTail, j.targetClosed, j.c.seg)
					} else {
						failAccepted(j.c.batch, j.c.responses, j.c.accepted, cerr)
					}
					replyAll(j.c.batch, j.c.responses)
					j.st.pending.Add(-1)
				})
			}
		}

		sc.mu.Lock()
		if len(sc.pending) == 0 {
			sc.flushing = false
			sc.mu.Unlock()
			return
		}
		jobs = sc.takeBatchLocked()
		sc.mu.Unlock()
	}
}

func (sc *sharedCommitter) close() error {
	sc.mu.Lock()
	sc.closed = true
	stray := sc.pending
	sc.pending = nil
	sc.mu.Unlock()
	for _, j := range stray {
		failJob(j, errStoreClosed)
	}
	// Wait for any in-flight runFlush to finish before the notifier shuts
	// down, and before Server.Close frees the native store handle it might
	// still be calling into.
	sc.wg.Wait()
	sc.notifier.shutdown()
	return nil
}
