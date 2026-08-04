package ds

import (
	"sync"
	"sync/atomic"
	"time"
)

// dormancyTimeout is how long a streamer stays resident with no activity
// before it exits and removes itself from the registry.
const dormancyTimeout = 60 * time.Second

// appendReq is a mutation submitted to a streamer. All validation and the
// write happen on the streamer goroutine, which gives per-(stream,
// producerId) serialisation for free.
type appendReq struct {
	records     [][]byte // record payloads (already JSON-flattened); empty for close-only
	hasBody     bool
	contentType string // request media type; empty allowed for close-only
	closeStream bool   // Stream-Closed: true
	streamSeq   *string
	producer    *producerHeaders
	resp        chan appendResp
}

type appendResp struct {
	status     int
	nextOffset Offset
	closed     bool

	producerEpoch *uint64
	producerSeq   *uint64
	expectedSeq   *uint64
	receivedSeq   *uint64

	err error
}

// streamer owns one live stream: it serialises appends, assigns sequence
// numbers, and broadcasts commits to readers.
type streamer struct {
	srv  *Server
	path string
	id   uint64

	// Immutable after construction; safe to read from any goroutine.
	contentType string
	isJSON      bool
	forkedFrom  uint64 // source stream id, 0 if not a fork
	forkOffset  uint64 // seq divergence point (fork owns [forkOffset, tail))

	reqs  chan appendReq
	touch chan struct{} // sliding-TTL keep-alive signal from readers
	stop  chan struct{}

	// mu guards enqueue against dormancy retirement / explicit stop.
	mu   sync.Mutex
	dead bool

	// Goroutine-owned state (no external access).
	meta            *StreamMeta
	producers       map[string]producerState
	producerLoaded  map[string]bool
	writerSeq       string
	writerSeqLoaded bool
	// writeTail is the writer-view tail. It advances as soon as a burst is
	// planned (for seq assignment and validation continuity), running
	// ahead of the reader-visible `tail` in notifier mode until the write
	// is durable.
	writeTail uint64

	// pending counts in-flight durability callbacks (notifier mode). A
	// streamer must not retire while this is > 0, or a respawn could read
	// a stale durable tail.
	pending atomic.Int64

	// Shared, lock-free reader state.
	tail            atomic.Uint64                 // next seq to assign / tail offset
	closed          atomic.Bool                   // cached closure flag
	longPollReaders atomic.Int64                  // parked long-poll readers
	sseReaders      atomic.Int64                  // parked SSE readers
	waiters         atomic.Pointer[chan struct{}] // broadcast channel; closed on commit

	waker     *liveWaker   // coalesces commit wakeups (see livewaker.go)
	wakeCount atomic.Int64 // broadcasts fired (observability / bench)
	cache     *liveCache   // in-memory recent-records cache; nil for forks (livecache.go)

	committer committer // durability strategy (sync / notifier), injected from the Server
}

// newStreamer constructs (but does not start) a streamer from loaded state.
func newStreamer(srv *Server, path string, meta *StreamMeta, tailSeq uint64) *streamer {
	s := &streamer{
		srv:            srv,
		path:           path,
		id:             meta.StreamID,
		contentType:    meta.ContentType,
		isJSON:         isJSONStream(meta.ContentType),
		forkedFrom:     meta.ForkedFrom,
		forkOffset:     meta.ForkOffset,
		reqs:           make(chan appendReq, maxBurst),
		touch:          make(chan struct{}, 1),
		stop:           make(chan struct{}),
		meta:           meta,
		producers:      make(map[string]producerState),
		producerLoaded: make(map[string]bool),
		committer:      srv.committer,
	}
	s.tail.Store(tailSeq)
	s.writeTail = tailSeq
	s.closed.Store(meta.Closed)
	ch := make(chan struct{})
	s.waiters.Store(&ch)
	s.waker = newLiveWaker(s, srv.cfg.LiveCoalesceWindow)
	// Forks read by stitching inherited source segments (resolveSegments).
	// The in-memory record cache serves only a stream's own contiguous
	// tail, so it is enabled for non-fork streams, and forks stay on the
	// readRange path.
	if meta.ForkedFrom == 0 && !srv.cfg.DisableLiveCache {
		s.cache = newLiveCache(srv.cfg.LiveCacheMaxRecords, srv.cfg.LiveCacheMaxBytes)
	}
	return s
}

// run is the streamer goroutine.
func (s *streamer) run() {
	idle := s.srv.cfg.DormancyTimeout
	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		select {
		case req := <-s.reqs:
			// Group commit: fold any already-queued appends into this one,
			// so a burst shares a single durability round-trip.
			s.processBurst(s.drainBurst(req))
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)
		case <-s.touch:
			s.refreshTTLOnRead()
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)
		case <-timer.C:
			if s.tryRetire() {
				return
			}
			timer.Reset(idle)
		case <-s.stop:
			// The streamer was force-retired (e.g. delete). Answer
			// anything that raced into the buffer, so no handler hangs.
			s.waker.stop()
			for {
				select {
				case req := <-s.reqs:
					req.resp <- appendResp{status: 404}
				default:
					return
				}
			}
		}
	}
}

// tryRetire removes the streamer from the registry after idle timeout. It
// aborts if work is pending. Once it marks the streamer dead under mu, no
// enqueue can race in: submit() checks dead under the same lock.
func (s *streamer) tryRetire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reqs) > 0 || s.totalReaders() > 0 || s.pending.Load() > 0 {
		return false
	}
	s.srv.unregister(s)
	s.dead = true
	s.waker.stop()
	return true
}

// waiterChan returns the current broadcast channel. It is closed when the
// next commit lands, waking readers parked in select.
func (s *streamer) waiterChan() <-chan struct{} { return *s.waiters.Load() }

// totalReaders returns the number of parked live readers across both
// long-poll and SSE, for call sites that only care whether any are
// present.
func (s *streamer) totalReaders() int64 {
	return s.longPollReaders.Load() + s.sseReaders.Load()
}

// stopped reports whether the streamer has been force-retired (deletion or
// shutdown). Live reads check this before serving scanned records, so they
// never hand back data for a stream whose deletion has begun.
func (s *streamer) stopped() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

// broadcast publishes a fresh waiter channel and closes the previous one,
// waking every parked reader. This is routed through s.waker (see
// applyReader), which may coalesce a burst of commits into fewer
// broadcasts.
func (s *streamer) broadcast() {
	next := make(chan struct{})
	old := s.waiters.Swap(&next)
	close(*old)
	s.wakeCount.Add(1)
}

// cacheRead tries to assemble a live read from the in-memory record cache,
// avoiding a store scan. ok=false means the caller must fall back to
// readRange: no cache, a fork, behind the retained window, or a case the
// cache will not serve.
func (s *streamer) cacheRead(dst []byte, isJSON bool, start Offset, tailSeq, limit uint64) (body []byte, next Offset, upToDate, ok bool) {
	if s.cache == nil {
		return nil, start, false, false
	}
	return s.cache.tryRead(dst, isJSON, start, tailSeq, limit)
}

// ttlRefreshOps returns the batch ops that slide this stream's TTL
// deadline forward, mutating the cached meta.Deadline. It returns nil when
// there is no sliding TTL, or a rewrite is not yet due (rate-limited to one
// write per granularity window). Only sliding TTL is refreshed;
// Stream-Expires-At is absolute. The written deadline is padded past
// now+TTL, so the stream never expires early despite sub-second
// truncation.
func (s *streamer) ttlRefreshOps(nowUnix int64) []Op {
	if s.meta.TTLSeconds == 0 {
		return nil
	}
	nowTTL := uint64(nowUnix) + s.meta.TTLSeconds
	if nowTTL < s.meta.Deadline {
		return nil
	}
	gran := s.meta.TTLSeconds / 4
	if gran < 1 {
		gran = 1
	} else if gran > 600 {
		gran = 600
	}
	old := s.meta.Deadline
	s.meta.Deadline = nowTTL + gran
	ops := []Op{{Key: expiryKey(s.meta.Deadline, s.id), Val: u64Value(s.meta.TTLSeconds)}}
	if old > 0 && old != s.meta.Deadline {
		ops = append(ops, Op{Key: expiryKey(old, s.id), Del: true})
	}
	return ops
}

// refreshTTLOnRead slides the TTL forward for a read. This is best-effort
// and non-durable: a lost refresh only shortens the window slightly. It
// runs on the streamer goroutine, so meta stays single-writer.
func (s *streamer) refreshTTLOnRead() {
	old := s.meta.Deadline
	ops := s.ttlRefreshOps(time.Now().Unix())
	if ops == nil {
		return
	}
	ops = append(ops, Op{Key: metaKey(s.path), Val: s.meta.Marshal()})
	if err := s.srv.store.Commit(ops, false); err != nil {
		s.meta.Deadline = old
	}
}

// loadProducer returns the cached (or DB-loaded) state for a producer id.
func (s *streamer) loadProducer(id string) (producerState, bool) {
	if s.producerLoaded[id] {
		st, ok := s.producers[id]
		return st, ok
	}
	s.producerLoaded[id] = true
	v, found, err := s.srv.store.Get(producerKey(s.id, id))
	if err != nil || !found {
		return producerState{}, false
	}
	st, err := unmarshalProducer(v)
	if err != nil {
		return producerState{}, false
	}
	s.producers[id] = st
	return st, true
}

func (s *streamer) loadWriterSeq() string {
	if s.writerSeqLoaded {
		return s.writerSeq
	}
	s.writerSeqLoaded = true
	if v, found, err := s.srv.store.Get(writerSeqKey(s.id)); err == nil && found {
		s.writerSeq = string(v)
	}
	return s.writerSeq
}

// maxBurst bounds how many queued appends fold into one group commit, and
// sizes the reqs channel buffer (newStreamer). The two move together, since
// drainBurst can never see more than the channel holds. This caps only
// yasdb's own per-stream batching, and is unrelated to SlateDB's own WAL
// flush cadence.
//
// This is a PER-STREAM allocation made unconditionally at spawn, so it
// must be sized for resident-stream COUNT, not single-stream throughput.
// appendReq is 80 bytes, and raising this to 4096 once, safe for one hot
// stream, meant 10 GiB of channel buffer at 32768 resident streams: more
// than an 8GB box has. 512 keeps most of the single-stream win at about
// 1/8th the memory. Check size × max-realistic-resident-count before
// raising this.
const maxBurst = 512

// drainBurst collects `first` plus any already-queued appends (non-blocking).
func (s *streamer) drainBurst(first appendReq) []appendReq {
	batch := []appendReq{first}
	for len(batch) < maxBurst {
		select {
		case r := <-s.reqs:
			batch = append(batch, r)
		default:
			return batch
		}
	}
	return batch
}

// pendingBurst accumulates the in-memory effects of a burst of appends
// before a single atomic durable commit. Committed state is only mutated
// after the commit succeeds, so a failed commit needs no rollback: the
// pending view is just dropped.
type pendingBurst struct {
	tail      uint64
	closed    bool
	deadline  uint64
	producers map[string]producerState // overlay of producers modified this burst
	writerSeq *string                  // set when Stream-Seq advanced this burst
	recordOps []Op
	now       int64
}

// processBurst validates each append in order against a shared pending
// view, then lands all accepted effects in one atomic durable WriteBatch.
// Validation stays per-(stream,producerId) serialised, because it all runs
// on this goroutine.
func (s *streamer) processBurst(batch []appendReq) {
	baseSeq := s.writeTail // seq of the first record this burst assigns
	pend := &pendingBurst{
		tail:     s.writeTail, // writer view (== durable tail in sync mode)
		closed:   s.meta.Closed,
		deadline: s.meta.Deadline,
		now:      time.Now().Unix(),
	}
	responses := make([]appendResp, len(batch))
	accepted := make([]bool, len(batch))
	metaChanged := false

	for i := range batch {
		responses[i], accepted[i] = s.planAppend(pend, batch[i])
		if accepted[i] && batch[i].closeStream {
			metaChanged = true
		}
	}

	deadlineChanged := pend.deadline != s.meta.Deadline
	nothing := len(pend.recordOps) == 0 && !metaChanged && pend.writerSeq == nil &&
		pend.producers == nil && !deadlineChanged
	if nothing {
		for i := range batch {
			batch[i].resp <- responses[i]
		}
		return
	}

	// Assemble one batch from the final pending view (mutable keys carry only
	// their final value, so no duplicate-key writes within the batch).
	ops := pend.recordOps
	ops = append(ops, Op{Key: tailKey(s.id), Val: marshalTail(pend.tail, pend.now)})
	for pid, ps := range pend.producers {
		ops = append(ops, Op{Key: producerKey(s.id, pid), Val: marshalProducer(ps)})
	}
	if pend.writerSeq != nil {
		ops = append(ops, Op{Key: writerSeqKey(s.id), Val: []byte(*pend.writerSeq)})
	}
	if deadlineChanged {
		ops = append(ops, Op{Key: expiryKey(pend.deadline, s.id), Val: u64Value(s.meta.TTLSeconds)})
		if s.meta.Deadline > 0 {
			ops = append(ops, Op{Key: expiryKey(s.meta.Deadline, s.id), Del: true})
		}
		metaChanged = true
	}
	if metaChanged {
		m := *s.meta
		m.Closed = pend.closed
		m.Deadline = pend.deadline
		ops = append(ops, Op{Key: metaKey(s.path), Val: m.Marshal()})
	}

	// Capture the run of record payloads for the live cache (non-fork
	// streams with live readers). pend.recordOps holds only record ops,
	// contiguous ascending from baseSeq. This is skipped when no readers
	// are parked, so a pure-write stream pays nothing. A reader that
	// connects mid-burst just misses this once, and falls back to the
	// store.
	var seg cacheSeg
	if s.cache != nil && len(pend.recordOps) > 0 && s.totalReaders() > 0 {
		seg.firstSeq = baseSeq
		seg.payloads = make([][]byte, len(pend.recordOps))
		for i, op := range pend.recordOps {
			seg.payloads[i] = recordPayload(op.Val)
		}
	}

	s.committer.commit(s, commitBurst{
		pend: pend, ops: ops, batch: batch, responses: responses,
		accepted: accepted, metaChanged: metaChanged, seg: seg,
	})
}

// cacheSeg is the contiguous run of record payloads a burst committed. It
// is handed to the live cache when the reader-visible tail advances.
type cacheSeg struct {
	firstSeq uint64
	payloads [][]byte
}

// applyWriter publishes the writer-view state (used for validation + seq
// assignment of the next burst). Runs on the streamer goroutine.
func (s *streamer) applyWriter(pend *pendingBurst, metaChanged bool) {
	s.writeTail = pend.tail
	if metaChanged {
		s.meta.Closed = pend.closed
		s.meta.Deadline = pend.deadline
	}
	for pid, ps := range pend.producers {
		s.producers[pid] = ps
	}
	if pend.writerSeq != nil {
		s.writerSeq = *pend.writerSeq
		s.writerSeqLoaded = true
	}
}

// applyReader publishes the reader-visible state (tail, closure) and wakes
// live readers. In notifier mode this runs on the notifier goroutine once
// the write is durable. All fields it touches are atomics or the broadcast
// pointer.
func (s *streamer) applyReader(tail uint64, closed bool, seg cacheSeg) {
	// Populate the cache before publishing the tail, so any reader that
	// observes the new tail finds the records in RAM. Only cache while
	// readers are present; drop the window otherwise, so write-only
	// streams cost nothing.
	if s.cache != nil {
		if s.totalReaders() > 0 {
			s.cache.append(seg.firstSeq, seg.payloads)
		} else {
			s.cache.reset()
		}
	}
	s.tail.Store(tail)
	if closed {
		s.closed.Store(true)
	}
	// Same reasoning as the cache gate above: waking zero parked readers
	// costs a channel allocation (broadcast) or a timer arm for nothing. A
	// reader that connects mid-burst reads st.tail fresh before parking,
	// so it never misses data; it only needs a subsequent commit's wake,
	// once it is counted.
	if s.totalReaders() > 0 {
		s.waker.Wake()
	}
}

func replyAll(batch []appendReq, responses []appendResp) {
	for i := range batch {
		batch[i].resp <- responses[i]
	}
}

func failAccepted(batch []appendReq, responses []appendResp, accepted []bool, err error) {
	for i := range batch {
		if accepted[i] {
			responses[i] = appendResp{status: 500, err: err}
		}
	}
}

// effProducer returns the effective producer state within the burst (pending
// overlay first, then committed/DB state).
func (s *streamer) effProducer(pend *pendingBurst, id string) (producerState, bool) {
	if pend.producers != nil {
		if ps, ok := pend.producers[id]; ok {
			return ps, true
		}
	}
	return s.loadProducer(id)
}

func (s *streamer) effWriterSeq(pend *pendingBurst) string {
	if pend.writerSeq != nil {
		return *pend.writerSeq
	}
	return s.loadWriterSeq()
}

// planAppend validates one append against the pending view and, when accepted,
// records its effects into that view. It never touches committed state.
func (s *streamer) planAppend(pend *pendingBurst, req appendReq) (appendResp, bool) {
	// 1. Closed status has the highest precedence.
	if pend.closed {
		return s.planAppendClosed(pend, req)
	}
	// 2. Content-type must match when a body is present.
	if req.hasBody && !contentTypeMatches(s.meta.ContentType, req.contentType) {
		return appendResp{status: 409}, false
	}
	// 3a. Idempotent producer validation.
	var newProd *producerState
	if req.producer != nil {
		st, exists := s.effProducer(pend, req.producer.id)
		act := validateProducer(st, exists, *req.producer)
		switch act.kind {
		case pStaleEpoch:
			e := act.currentEpoch
			return appendResp{status: 403, producerEpoch: &e}, false
		case pBadSeqStart:
			return appendResp{status: 400}, false
		case pGap:
			exp, rcv := act.expectedSeq, act.receivedSeq
			return appendResp{status: 409, expectedSeq: &exp, receivedSeq: &rcv}, false
		case pDuplicate:
			// Echo the highest accepted seq, not the request's.
			e, sq := st.epoch, st.lastSeq
			return appendResp{status: 204, nextOffset: tailOffset(pend.tail), closed: pend.closed, producerEpoch: &e, producerSeq: &sq}, false
		case pAccept:
			ns := act.newState
			ns.closedBy = req.closeStream
			newProd = &ns
		}
	}
	// 3b. Stream-Seq must be strictly increasing (per-stream scope).
	if req.streamSeq != nil {
		if *req.streamSeq <= s.effWriterSeq(pend) {
			return appendResp{status: 409}, false
		}
	}

	// Accept: assign sequence and record the effects in the pending view.
	start := pend.tail
	n := uint64(len(req.records))
	for i, rec := range req.records {
		pend.recordOps = append(pend.recordOps, Op{Key: recordKey(s.id, start+uint64(i)), Val: marshalRecord(recFlagNone, rec)})
	}
	pend.tail = start + n
	if newProd != nil {
		if pend.producers == nil {
			pend.producers = make(map[string]producerState)
		}
		pend.producers[req.producer.id] = *newProd
	}
	if req.streamSeq != nil {
		v := *req.streamSeq
		pend.writerSeq = &v
	}
	if req.closeStream {
		pend.closed = true
	}
	pend.slideTTL(s.meta.TTLSeconds)

	status := 204
	var pEpoch, pSeq *uint64
	if req.producer != nil {
		e, sq := req.producer.epoch, req.producer.seq
		pEpoch, pSeq = &e, &sq
		if n > 0 { // new data via an idempotent producer -> 200
			status = 200
		}
	}
	return appendResp{status: status, nextOffset: tailOffset(start + n), closed: pend.closed, producerEpoch: pEpoch, producerSeq: pSeq}, true
}

// planAppendClosed handles appends to an already-closed stream (pending view).
func (s *streamer) planAppendClosed(pend *pendingBurst, req appendReq) (appendResp, bool) {
	if !req.hasBody {
		return appendResp{status: 204, nextOffset: tailOffset(pend.tail), closed: true}, false
	}
	if req.producer != nil {
		st, exists := s.effProducer(pend, req.producer.id)
		if validateProducer(st, exists, *req.producer).kind == pDuplicate {
			e, sq := st.epoch, st.lastSeq
			return appendResp{status: 204, nextOffset: tailOffset(pend.tail), closed: true, producerEpoch: &e, producerSeq: &sq}, false
		}
	}
	return appendResp{status: 409, nextOffset: tailOffset(pend.tail), closed: true}, false
}

// slideTTL advances the pending sliding-TTL deadline (rate-limited, padded).
func (pend *pendingBurst) slideTTL(ttlSecs uint64) {
	if ttlSecs == 0 {
		return
	}
	nowTTL := uint64(pend.now) + ttlSecs
	if nowTTL < pend.deadline {
		return
	}
	gran := ttlSecs / 4
	if gran < 1 {
		gran = 1
	} else if gran > 600 {
		gran = 600
	}
	pend.deadline = nowTTL + gran
}
