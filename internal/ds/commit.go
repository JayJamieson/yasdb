package ds

import (
	"runtime"
	"sync"
)

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
// committer worker folds into a single WriteBatch — distinct from maxBurst,
// which bounds one stream's own queued requests. See
// docs/rfcs/0001-cross-stream-group-commit.md.
const defaultCommitterBatchMax = 512

// commitJob is one streamer's already-planned burst, handed to a
// sharedCommitter worker. applyWriter has already run by the time a job
// reaches a worker, so a worker only ever does I/O — no streamer-owned state
// is touched off the streamer's own goroutine.
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

// sharedCommitter is the notifier-durability committer (RFC 0001). It is a
// small GOMAXPROCS-sized pool of workers. Instead of one CommitAsync call
// per stream's own burst, each worker coalesces bursts from whichever
// streams land in its queue into one shared WriteBatch. Throughput then
// scales with total system load, not per-stream concurrency.
//
// A stream is always routed to the same worker (hash by id), so that
// worker's single-goroutine FIFO drain preserves the ordering
// durabilityNotifier depends on: CommitAsync calls for one stream must
// reach SlateDB in the order that stream produced them, since fire() sorts
// callbacks by SlateDB's own returned sequence number, not call order (see
// durability.go). Different streams have no ordering relationship to each
// other, so folding them into one WriteBatch is always safe.
type sharedCommitter struct {
	store    Storage
	notifier *durabilityNotifier
	workers  []*committerWorker
	wg       sync.WaitGroup
}

// committerWorker owns one commit queue. mu and closed make "check
// not-closed, then enqueue" (commit) atomic with "mark closed, then close
// the channel" (close). This is the same race durabilityNotifier.subscribe
// already guards against (see its doc comment). Without that, a job could
// land in a channel a worker already stopped draining, and never resolve.
// This uses one mutex per worker, not one global mutex, so only jobs routed
// to the same worker ever contend.
type committerWorker struct {
	mu     sync.Mutex
	closed bool
	in     chan commitJob
}

func newSharedCommitter(store Storage, cfg Config) *sharedCommitter {
	n := cfg.CommitterWorkers
	if n <= 0 {
		n = runtime.GOMAXPROCS(0)
	}
	if n < 1 {
		n = 1
	}
	batchMax := cfg.CommitterBatchMax
	if batchMax <= 0 {
		batchMax = defaultCommitterBatchMax
	}

	sc := &sharedCommitter{
		store:    store,
		notifier: newDurabilityNotifier(store, cfg.NotifierPollInterval),
		workers:  make([]*committerWorker, n),
	}
	for i := range sc.workers {
		w := &committerWorker{in: make(chan commitJob, batchMax)}
		sc.workers[i] = w
		sc.wg.Add(1)
		go func() {
			defer sc.wg.Done()
			sc.runWorker(w, batchMax)
		}()
	}
	return sc
}

// fnv1aU64 hashes a uint64 with zero allocation, mirroring registry.go's
// fnv1a (which hashes a string path instead).
func fnv1aU64(id uint64) uint32 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	h := uint32(offset32)
	for i := 0; i < 8; i++ {
		h ^= uint32(byte(id >> (8 * i)))
		h *= prime32
	}
	return h
}

func (sc *sharedCommitter) workerFor(streamID uint64) *committerWorker {
	return sc.workers[fnv1aU64(streamID)%uint32(len(sc.workers))]
}

// commit does the Go-side bookkeeping synchronously on the streamer's own
// goroutine. applyWriter must run before this returns, since the next
// burst's baseSeq is read from st.writeTail immediately after. Only the
// actual CommitAsync call and durability tracking are expensive I/O, so
// only that part is handed to a worker.
//
// applyWriter runs even on the path that ends in failJob (the worker is
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

	w := sc.workerFor(st.id)
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		failJob(job, errStoreClosed)
		return
	}
	w.in <- job
	w.mu.Unlock()
}

// runWorker drains its queue, folding whatever jobs are already waiting,
// from any stream, into one CommitAsync call, up to batchMax. All jobs in
// one call share a SlateDB sequence number, which is fine: subscribe is
// called per job in drain order (FIFO), and equal-target subscribers fire
// in that same order (subscribe's tie-breaking). So two jobs for the same
// stream still resolve in submission order.
//
// Ranging over w.in is safe until close: shutdown only closes it under
// w.mu, the same lock commit() checks before sending. So nothing lands
// after this loop sees it close.
func (sc *sharedCommitter) runWorker(w *committerWorker, batchMax int) {
	for job := range w.in {
		sc.processBatch(w, job, batchMax)
	}
}

func (sc *sharedCommitter) processBatch(w *committerWorker, first commitJob, batchMax int) {
	// This grows from a small guess. It is not pre-allocated at batchMax:
	// pprof at 32768 streams found that pre-allocating 512 commitJobs on
	// every call, regardless of how many actually arrived, spent 18%+ of
	// total CPU zeroing unused slots on the common case of a batch of 1-2.
	jobs := make([]commitJob, 1, 8)
	jobs[0] = first
	for len(jobs) < batchMax {
		select {
		case j := <-w.in:
			jobs = append(jobs, j)
		default:
			goto commit
		}
	}
commit:
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
		return
	}
	for _, j := range jobs {
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

func (sc *sharedCommitter) close() error {
	// Mark closed and close the channel under the same mutex commit() checks
	// before sending. No send can land after close (see committerWorker's
	// doc comment).
	for _, w := range sc.workers {
		w.mu.Lock()
		w.closed = true
		close(w.in)
		w.mu.Unlock()
	}
	// Wait for every worker to drain and exit before the notifier shuts
	// down, and, more importantly, before Server.Close frees the native
	// store handle a worker might still be calling into.
	sc.wg.Wait()
	sc.notifier.shutdown()
	return nil
}
