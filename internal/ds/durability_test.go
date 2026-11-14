package ds

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pushStore is a Storage whose durable watermark advances a little after each
// commit, like SlateDB's WAL flush, and which can be waited on rather than
// polled. The randomized delay is what makes the probes below discriminating:
// it spreads acknowledgements out, so a notifier that published from more than
// one goroutine would interleave visibly.
type pushStore struct {
	mu        sync.Mutex
	seq       uint64
	durable   uint64
	durableCh chan struct{}
}

func newPushStore() *pushStore { return &pushStore{durableCh: make(chan struct{})} }

func (p *pushStore) Get([]byte) ([]byte, bool, error)      { return nil, false, nil }
func (p *pushStore) Commit([]Op, bool) error               { return nil }
func (p *pushStore) Scan([]byte, []byte) (Iterator, error) { return nil, nil }
func (p *pushStore) Close() error                          { return nil }

func (p *pushStore) advance(seq uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if seq > p.durable {
		p.durable = seq
		close(p.durableCh)
		p.durableCh = make(chan struct{})
	}
}

func (p *pushStore) CommitAsync([]Op) (uint64, error) {
	p.mu.Lock()
	p.seq++
	seq := p.seq
	p.mu.Unlock()
	go func() {
		time.Sleep(time.Duration(rand.Intn(3000)) * time.Microsecond)
		p.advance(seq)
	}()
	return seq, nil
}

func (p *pushStore) DurableSeq() (uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.durable, nil
}

func (p *pushStore) WatchDurable(after uint64, stop <-chan struct{}) (uint64, error) {
	budget := time.NewTimer(watchTimeout)
	defer budget.Stop()
	for {
		p.mu.Lock()
		durable, ch := p.durable, p.durableCh
		p.mu.Unlock()
		if durable > after {
			return durable, nil
		}
		select {
		case <-ch:
		case <-stop:
			return durable, nil
		case <-budget.C:
			return durable, nil
		}
	}
}

// unboundedStore watches its own watermark with no timeout at all, and
// returns only when it advances or stop closes. Every other double here
// bounds itself with watchTimeout, which hides whether shutdown can actually
// interrupt a watch. This one is the conforming backend an embedder writes
// from the public DurabilitySubscriber contract, where watchTimeout is not a
// value they can see.
type unboundedStore struct{ *pushStore }

func (u unboundedStore) WatchDurable(after uint64, stop <-chan struct{}) (uint64, error) {
	for {
		u.mu.Lock()
		durable, ch := u.durable, u.durableCh
		u.mu.Unlock()
		if durable > after {
			return durable, nil
		}
		select {
		case <-ch:
		case <-stop:
			return durable, nil
		}
	}
}

// instantStore is durable the moment CommitAsync returns, like fakeStore and
// pwalStore. Every target is therefore already satisfied when it is
// subscribed, which is the interleaving the 2026-07-29 tail-regression bug
// lived in.
type instantStore struct{ *pushStore }

func (p instantStore) CommitAsync([]Op) (uint64, error) {
	p.mu.Lock()
	p.seq++
	seq := p.seq
	p.mu.Unlock()
	p.advance(seq)
	return seq, nil
}

// pollOnlyStore hides pushStore's WatchDurable behind the Storage interface,
// so newDurabilityNotifier installs pollWatchDurable instead. This is every
// backend that is not SlateDB: pwalStore, fakeStore, an embedder's own
// Storage, and any decorator that embeds the interface.
type pollOnlyStore struct{ Storage }

// countingStore records how the notifier reaches the backend: whether it
// waits or polls, and whether a call is in flight.
type countingStore struct {
	*pushStore
	watchCalls atomic.Int64
	seqCalls   atomic.Int64
	inFlight   atomic.Int64
}

func (c *countingStore) WatchDurable(after uint64, stop <-chan struct{}) (uint64, error) {
	c.watchCalls.Add(1)
	c.inFlight.Add(1)
	defer c.inFlight.Add(-1)
	return c.pushStore.WatchDurable(after, stop)
}

func (c *countingStore) DurableSeq() (uint64, error) {
	c.seqCalls.Add(1)
	c.inFlight.Add(1)
	defer c.inFlight.Add(-1)
	return c.pushStore.DurableSeq()
}

// probeStream stands in for a streamer. applyReader publishes the
// reader-visible tail with a blind atomic Store and no max, exactly as
// streamer.applyReader does, so any out-of-order or concurrent callback
// shows up as a regression.
type probeStream struct {
	tail        atomic.Uint64
	regressions atomic.Int64
}

func (s *probeStream) applyReader(tail uint64) {
	// The real applyReader appends to the live cache and wakes readers before
	// it stores the tail. The delay stands in for that work, widening the
	// window a multi-publisher design would lose in.
	time.Sleep(time.Duration(rand.Intn(60)) * time.Microsecond)
	prev := s.tail.Load()
	s.tail.Store(tail)
	if tail < prev {
		s.regressions.Add(1)
	}
}

// probeFlusher mirrors sharedCommitter: one physical CommitAsync at a time,
// with every job in a drained batch subscribed under the one returned seq, in
// drain order.
type probeFlusher struct {
	store    Storage
	notifier *durabilityNotifier

	mu       sync.Mutex
	pending  []probeJob
	flushing bool
	wg       sync.WaitGroup
}

type probeJob struct {
	st   *probeStream
	tail uint64
	done func()
}

func (f *probeFlusher) commit(j probeJob) {
	f.mu.Lock()
	f.pending = append(f.pending, j)
	if f.flushing {
		f.mu.Unlock()
		return
	}
	f.flushing = true
	batch := f.pending
	f.pending = nil
	f.wg.Add(1)
	f.mu.Unlock()
	go f.run(batch)
}

func (f *probeFlusher) run(jobs []probeJob) {
	defer f.wg.Done()
	for {
		seq, err := f.store.CommitAsync(nil)
		if err != nil {
			panic(err)
		}
		for _, j := range jobs {
			j := j
			if serr := f.notifier.subscribe(seq, func(cerr error) {
				if cerr == nil {
					j.st.applyReader(j.tail)
				}
				j.done()
			}); serr != nil {
				j.done()
			}
		}
		f.mu.Lock()
		if len(f.pending) == 0 {
			f.flushing = false
			f.mu.Unlock()
			return
		}
		jobs = f.pending
		f.pending = nil
		f.mu.Unlock()
	}
}

// probeStreamerLoop mirrors streamer.run: one goroutine per stream assigns
// tails and hands bursts to the committer, so commit calls for one stream are
// strictly tail-ordered. That discipline is the real system's; the notifier is
// only responsible for preserving it.
func probeStreamerLoop(f *probeFlusher, st *probeStream, reqs <-chan chan uint64, done chan<- struct{}) {
	var tail uint64
	for r := range reqs {
		tail++
		t := tail
		reply := r
		f.commit(probeJob{st: st, tail: t, done: func() { reply <- t }})
	}
	close(done)
}

type probeResult struct {
	violations  int64
	regressions int64
	finalTail   uint64
}

// runNotifierOrderProbe drives n with producers concurrent writers against one
// stream and counts read-after-ack violations: an offset was acknowledged to
// some producer before a read started, yet the read saw a smaller tail.
func runNotifierOrderProbe(t *testing.T, store Storage, n *durabilityNotifier, producers, perProd int) probeResult {
	t.Helper()
	f := &probeFlusher{store: store, notifier: n}
	st := &probeStream{}
	reqs := make(chan chan uint64, 64)
	loopDone := make(chan struct{})
	go probeStreamerLoop(f, st, reqs, loopDone)

	var acked atomic.Uint64
	var violations atomic.Int64
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reply := make(chan uint64, 1)
			for i := 0; i < perProd; i++ {
				reqs <- reply
				tail := <-reply
				for {
					cur := acked.Load()
					if tail <= cur || acked.CompareAndSwap(cur, tail) {
						break
					}
				}
				// Some offset is known acknowledged before this read starts.
				expected := acked.Load()
				if got := st.tail.Load(); got < expected {
					violations.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	close(reqs)
	<-loopDone
	f.wg.Wait()
	n.shutdown()

	return probeResult{
		violations:  violations.Load(),
		regressions: st.regressions.Load(),
		finalTail:   st.tail.Load(),
	}
}

func assertOrdered(t *testing.T, got probeResult, want uint64) {
	t.Helper()
	if got.violations != 0 {
		t.Errorf("read-after-ack violations: %d", got.violations)
	}
	if got.regressions != 0 {
		t.Errorf("reader-visible tail regressed %d times", got.regressions)
	}
	if got.finalTail != want {
		t.Errorf("final tail = %d, want %d", got.finalTail, want)
	}
}

const (
	probeProducers = 8
	probePerProd   = 200
)

// TestNotifierPublishesInAckOrder is the ordering probe for the push path: a
// backend whose watermark advances asynchronously after each commit.
func TestNotifierPublishesInAckOrder(t *testing.T) {
	s := newPushStore()
	got := runNotifierOrderProbe(t, s, newDurabilityNotifier(s, time.Millisecond), probeProducers, probePerProd)
	assertOrdered(t, got, probeProducers*probePerProd)
}

// TestNotifierPublishesInAckOrderWhenAlreadyDurable is the same probe against a
// backend that is durable the instant CommitAsync returns, so every target is
// already satisfied when it is subscribed. This is the direct regression test
// for the 2026-07-29 bug, where subscribe fired such a target inline on the
// caller's goroutine and raced the poller firing an earlier burst.
func TestNotifierPublishesInAckOrderWhenAlreadyDurable(t *testing.T) {
	s := instantStore{newPushStore()}
	got := runNotifierOrderProbe(t, s, newDurabilityNotifier(s, time.Millisecond), probeProducers, probePerProd)
	assertOrdered(t, got, probeProducers*probePerProd)
}

// TestNotifierPublishesInAckOrderWhenPolled is the same probe against a backend
// with no push capability, which takes the pollWatchDurable fallback.
func TestNotifierPublishesInAckOrderWhenPolled(t *testing.T) {
	s := pollOnlyStore{newPushStore()}
	got := runNotifierOrderProbe(t, s, newDurabilityNotifier(s, time.Millisecond), probeProducers, probePerProd)
	assertOrdered(t, got, probeProducers*probePerProd)
}

// newMultiWatcherNotifier builds the anti-design: k goroutines each waiting on
// the store and firing whatever their own observation covers. It is the shape
// someone reaches for when they "parallelise" the notifier, and it is the shape
// the 2026-07-29 bug had.
func newMultiWatcherNotifier(store Storage, k int) *durabilityNotifier {
	n := &durabilityNotifier{
		wake: make(chan struct{}, 1),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	sub := store.(DurabilitySubscriber)
	n.watch = sub.WatchDurable
	var wg sync.WaitGroup
	for i := 0; i < k; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var last uint64
			for {
				select {
				case <-n.stop:
					n.closeAll(errStoreClosed)
					return
				default:
				}
				n.fire(last)
				durable, err := sub.WatchDurable(last, n.stop)
				if err != nil {
					n.closeAll(err)
					return
				}
				if durable > last {
					last = durable
				}
			}
		}()
	}
	go func() { wg.Wait(); close(n.done) }()
	return n
}

// TestNotifierOrderProbeHasTeeth is a negative control: it runs the same probe
// against the multi-watcher anti-design and asserts the probe rejects it.
// Without this, the three ordering tests above prove nothing, since a probe
// that accepts everything would also pass them.
//
// It is also the guard against someone later "optimising" the notifier into
// several watcher goroutines, or reintroducing an inline fire in subscribe.
func TestNotifierOrderProbeHasTeeth(t *testing.T) {
	s := newPushStore()
	got := runNotifierOrderProbe(t, s, newMultiWatcherNotifier(s, 4), probeProducers, probePerProd)
	bad := got.violations + got.regressions
	t.Logf("4 watcher goroutines: %d read-after-ack violations, %d tail regressions", got.violations, got.regressions)
	if bad == 0 {
		t.Fatal("the probe accepted out-of-order publishing, so it proves nothing")
	}
}

// TestNotifierDoesNotPollAPushCapableStore is the prize: with a backend that
// implements DurabilitySubscriber, an outstanding acknowledgement is waited
// on, not polled. The old notifier called DurableSeq once per millisecond for
// the whole wait.
func TestNotifierDoesNotPollAPushCapableStore(t *testing.T) {
	s := &countingStore{pushStore: newPushStore()}
	n := newDurabilityNotifier(s, time.Millisecond)
	defer n.shutdown()

	fired := make(chan error, 1)
	if err := n.subscribe(1, func(err error) { fired <- err }); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// Hold the acknowledgement outstanding long enough that a 1ms poll would
	// have made hundreds of DurableSeq calls.
	time.Sleep(100 * time.Millisecond)
	s.advance(1)

	select {
	case err := <-fired:
		if err != nil {
			t.Fatalf("callback failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("callback never fired")
	}
	if got := s.seqCalls.Load(); got != 0 {
		t.Errorf("DurableSeq called %d times on a push-capable store, want 0", got)
	}
	if got := s.watchCalls.Load(); got < 1 {
		t.Errorf("WatchDurable called %d times, want at least 1", got)
	}
}

// TestNotifierFiresAnAlreadyDurableTargetAtOnce pins the latency of a waiter
// that is due the moment it is registered. run invokes callbacks outside the
// lock, so a waiter can register after fire has read the waiter slice, with a
// target the watermark already covers. Without the already-due guard in run,
// the next watch asks for an advance past a watermark already reached and the
// acknowledgement waits out watchTimeout.
//
// sharedCommitter does not reach this: it keeps one CommitAsync in flight, so
// the targets it subscribes never decrease. The waiter here is registered the
// way a caller without that property would register one.
func TestNotifierFiresAnAlreadyDurableTargetAtOnce(t *testing.T) {
	s := newPushStore()
	n := newDurabilityNotifier(s, time.Millisecond)
	defer n.shutdown()

	// Target 10 holds run inside watch until the watermark jumps to 100, one
	// flush covering several writes. run then fires it, and target 50 is
	// subscribed from inside that callback, already covered by the 100.
	late := make(chan time.Duration, 1)
	if err := n.subscribe(10, func(error) {
		t0 := time.Now()
		_ = n.subscribe(50, func(error) { late <- time.Since(t0) })
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	time.Sleep(20 * time.Millisecond)
	s.advance(100)

	select {
	case d := <-late:
		t.Logf("already-durable waiter (target 50, watermark 100) fired after %v", d)
		// The guarded path fires in microseconds and the unguarded one takes
		// watchTimeout, so 50ms separates them with room for scheduler noise.
		if d > 50*time.Millisecond {
			t.Errorf("an acknowledgement whose target was already durable was delayed %v, want under 50ms", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("already-durable waiter never fired")
	}
}

// TestNotifierIdleMakesNoStoreCalls pins the idle cost. With no acknowledgement
// outstanding there is nothing to wait for, so the notifier parks and touches
// the store zero times.
func TestNotifierIdleMakesNoStoreCalls(t *testing.T) {
	s := &countingStore{pushStore: newPushStore()}
	n := newDurabilityNotifier(s, time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	watch, seq := s.watchCalls.Load(), s.seqCalls.Load()
	n.shutdown()
	if watch != 0 || seq != 0 {
		t.Errorf("idle notifier made %d WatchDurable and %d DurableSeq calls, want 0 and 0", watch, seq)
	}
}

// TestNotifierShutdownJoinsTheWatcher pins shutdown's postcondition: when it
// returns, no goroutine is inside a Storage call and every outstanding waiter
// has been failed. Server.Close frees the native store handle right after
// this, and the watcher can be parked inside an FFI call.
func TestNotifierShutdownJoinsTheWatcher(t *testing.T) {
	s := &countingStore{pushStore: newPushStore()}
	n := newDurabilityNotifier(s, time.Millisecond)

	var failed atomic.Int64
	if err := n.subscribe(1<<40, func(err error) {
		if err != nil {
			failed.Add(1)
		}
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if s.inFlight.Load() == 0 {
		t.Fatal("the watcher never entered the store, so this run proves nothing about the join")
	}

	start := time.Now()
	n.shutdown()
	elapsed := time.Since(start)

	if got := s.inFlight.Load(); got != 0 {
		t.Errorf("shutdown returned with %d store calls still in flight", got)
	}
	if got := failed.Load(); got != 1 {
		t.Errorf("shutdown failed %d of 1 outstanding waiters", got)
	}
	t.Logf("shutdown joined the watcher in %v", elapsed)
	if elapsed > 2*watchTimeout {
		t.Errorf("shutdown took %v, want at most %v", elapsed, 2*watchTimeout)
	}
}

// TestNotifierShutdownInterruptsAnUnboundedWatch is the same postcondition for
// a backend that does not bound its own wait. shutdown's join cannot be
// interrupted from the outside, so the stop channel WatchDurable receives is
// the only way out. Without it, DB.Close hangs for as long as the backend's
// watermark stays put, which is exactly when an operator reaches for Close.
func TestNotifierShutdownInterruptsAnUnboundedWatch(t *testing.T) {
	s := unboundedStore{newPushStore()}
	n := newDurabilityNotifier(s, time.Millisecond)

	var failed atomic.Int64
	if err := n.subscribe(1<<40, func(err error) {
		if err != nil {
			failed.Add(1)
		}
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	// shutdown runs on its own goroutine so an unbounded watch fails this
	// test instead of hanging the package until the go test timeout.
	done := make(chan time.Duration, 1)
	start := time.Now()
	go func() { n.shutdown(); done <- time.Since(start) }()
	select {
	case d := <-done:
		t.Logf("shutdown interrupted an unbounded watch in %v", d)
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown never returned: the watch outlasted stop and nothing else can interrupt it")
	}
	if got := failed.Load(); got != 1 {
		t.Errorf("shutdown failed %d of 1 outstanding waiters", got)
	}
}

// TestNotifierSubscribeAfterShutdownFails pins the other half of subscribe's
// contract. It never invokes the callback itself, so an already-closed
// notifier reports the error to the caller rather than running a callback on
// the caller's goroutine.
func TestNotifierSubscribeAfterShutdownFails(t *testing.T) {
	s := newPushStore()
	n := newDurabilityNotifier(s, time.Millisecond)
	n.shutdown()

	var ran atomic.Bool
	err := n.subscribe(1, func(error) { ran.Store(true) })
	if err == nil {
		t.Fatal("subscribe on a shut-down notifier must return an error")
	}
	if ran.Load() {
		t.Error("subscribe invoked the callback on the caller's goroutine")
	}
}
