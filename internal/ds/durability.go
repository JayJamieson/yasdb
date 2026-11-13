package ds

import (
	"sort"
	"sync"
	"time"
)

// watchDurable blocks until the store's durable watermark is known to exceed
// after, then returns it. It may also return early with a watermark at or
// below after, meaning "nothing new yet"; the caller loops.
//
// The returned watermark is always a value the backend actually reported as
// durable, never a floor the caller already knew. durabilityNotifier carries
// it forward as the baseline it acknowledges against, so a watermark the
// backend never reported would acknowledge a write that is not durable.
//
// A closed stop means the notifier is shutting down, and the call must return
// promptly. shutdown joins run, and run can be parked in here, so a call that
// ignores stop without bounding itself some other way wedges Server.Close.
//
// An error means the store is closed or has failed, and the watermark is then
// meaningless. An implementation need not be safe for concurrent use: the
// notifier calls this from exactly one goroutine, and that is the same
// goroutine that invokes every callback.
type watchDurable func(after uint64, stop <-chan struct{}) (durable uint64, err error)

// durabilityNotifier acknowledges writes once the engine's durable watermark
// reaches their assigned sequence number, instead of blocking each write on
// an fsync. This is the "durability notifier" design, the one s2-lite uses.
// One notifier serves every stream: appends never block on durability, so a
// single stream pipelines many appends into one flush. yasdb uses this when
// Config.Durability == "notifier".
//
// SlateDB v0.18 pushes durability updates through a status subscription, so
// there is no poll here for that backend. A backend without the capability
// (pwalStore, the test fake, an embedder's own Storage) gets a DurableSeq
// poll loop behind the same watchDurable signature.
type durabilityNotifier struct {
	// watch is the push subscription when the store has one, and a
	// DurableSeq poll loop otherwise. Called only from run.
	watch watchDurable

	mu      sync.Mutex
	waiters []durWaiter // sorted ascending by target; ties in subscribe order
	closed  bool

	// lastDurable is the highest watermark watch has reported. Owned
	// exclusively by run: no lock, and no other goroutine touches it.
	lastDurable uint64

	wake chan struct{}
	stop chan struct{}
	done chan struct{} // closed by run on exit; shutdown joins on it

	stopOnce sync.Once
}

// durWaiter is one outstanding acknowledgement. target is the engine sequence
// number the write was assigned, not a stream tail; the callback closes over
// the tail it publishes.
type durWaiter struct {
	target uint64
	cb     func(error)
}

// newDurabilityNotifier starts the notifier over store. pollInterval applies
// only when store does not implement DurabilitySubscriber.
func newDurabilityNotifier(store Storage, pollInterval time.Duration) *durabilityNotifier {
	n := &durabilityNotifier{
		wake: make(chan struct{}, 1),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if sub, ok := store.(DurabilitySubscriber); ok {
		n.watch = sub.WatchDurable
	} else {
		n.watch = pollWatchDurable(store, pollInterval)
	}
	go n.run()
	return n
}

// subscribe arranges for cb to run once the durable watermark reaches target.
//
// cb(nil) ALWAYS runs on the notifier's own run goroutine, never here, even
// when target is already durable. Success callbacks are therefore totally
// ordered: by target, and for equal targets by the order they were
// subscribed, since the sort.Search below uses a strict > so an equal target
// inserts after its peers, and every job in one runFlush batch shares one
// target. applyReader depends on that total order: it publishes st.tail with
// a blind Store, so two callbacks running concurrently, or in the wrong
// order, regress a stream's tail and break read-after-ack. That was a real
// bug, fixed 2026-07-29.
//
// subscribe returns errStoreClosed, without registering cb, when the notifier
// is already shut down. It never invokes cb itself, so a callback can only
// ever run on run's goroutine.
func (n *durabilityNotifier) subscribe(target uint64, cb func(error)) error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return errStoreClosed
	}
	i := sort.Search(len(n.waiters), func(i int) bool { return n.waiters[i].target > target })
	n.waiters = append(n.waiters, durWaiter{})
	copy(n.waiters[i+1:], n.waiters[i:])
	n.waiters[i] = durWaiter{target: target, cb: cb}
	n.mu.Unlock()
	select {
	case n.wake <- struct{}{}:
	default:
	}
	return nil
}

// run is the only goroutine that ever invokes a callback, and the only one
// that calls watch.
func (n *durabilityNotifier) run() {
	defer close(n.done)
	for {
		// Fire anything the last observation already covers. Under
		// sharedCommitter this is a no-op, because only one CommitAsync is
		// ever in flight, so targets reaching subscribe are non-decreasing
		// and a new waiter is always above lastDurable. It is here, and it
		// is cheap, so the notifier stays correct for a caller that does not
		// hold that property, instead of depending on an invariant that
		// lives in another file.
		n.fire(n.lastDurable)

		target, ok := n.lowestTarget()
		if !ok {
			// Nothing to wait for, so an idle server makes zero calls into
			// the store.
			select {
			case <-n.stop:
				n.closeAll(errStoreClosed)
				return
			case <-n.wake:
			}
			continue
		}
		select {
		case <-n.stop:
			n.closeAll(errStoreClosed)
			return
		default:
		}

		if target <= n.lastDurable {
			// Already due. The waiter registered after this iteration's fire
			// released the lock, so fire missed it, and watch would be asked
			// to advance past a watermark already reached. That wait costs a
			// full watchTimeout. Loop instead, and the fire above picks the
			// waiter up. It cannot spin, because that fire removes the
			// waiter whose target we just read.
			//
			// This narrows the window rather than closing it. A waiter that
			// arrives between here and watch below is still not seen until
			// that watch returns. But the window is now a few instructions
			// instead of fire's whole callback loop, which runs up to
			// defaultCommitterBatchMax callbacks. Closing it completely would
			// mean letting subscribe interrupt an in-flight watch, which
			// would pull the push path out of its subscription on every
			// append and cost more than the case is worth.
			continue
		}

		// One below the lowest outstanding target: the watermark only has to
		// reach that target for the next callback to be due. The guard above
		// leaves target above lastDurable, so target-1 can neither underflow
		// nor ask watch to go backwards.
		durable, err := n.watch(target-1, n.stop)
		if err != nil {
			n.closeAll(err)
			return
		}
		if durable > n.lastDurable {
			n.lastDurable = durable
		}
	}
}

func (n *durabilityNotifier) lowestTarget() (uint64, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.waiters) == 0 {
		return 0, false
	}
	return n.waiters[0].target, true
}

// fire invokes, in slice order and outside the lock, every waiter whose
// target the watermark covers. Called only from run.
func (n *durabilityNotifier) fire(durable uint64) {
	n.mu.Lock()
	i := 0
	for i < len(n.waiters) && n.waiters[i].target <= durable {
		i++
	}
	if i == 0 {
		// run calls fire on every iteration, and usually nothing is due.
		// Returning here keeps that case from rebuilding the waiter slice.
		n.mu.Unlock()
		return
	}
	ready := n.waiters[:i:i]
	n.waiters = append([]durWaiter(nil), n.waiters[i:]...)
	n.mu.Unlock()
	for _, w := range ready {
		w.cb(nil)
	}
}

func (n *durabilityNotifier) closeAll(err error) {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	n.closed = true
	pending := n.waiters
	n.waiters = nil
	n.mu.Unlock()
	for _, w := range pending {
		w.cb(err)
	}
}

// shutdown stops run and joins it, so every outstanding waiter has been
// failed and no goroutine is inside a Storage call by the time it returns.
// The join is what lets Server.Close free the native store handle: run can be
// parked inside an FFI call. Idempotent and safe to call concurrently.
//
// The join is unconditional on purpose. Returning on a timeout, with run
// still inside a store call, is the use-after-free Server.Close is ordered to
// prevent. So shutdown returns only as fast as watch does, which is why
// watchDurable requires a call to honour stop or bound itself.
func (n *durabilityNotifier) shutdown() {
	n.stopOnce.Do(func() { close(n.stop) })
	<-n.done
}

// pollWatchDurable is the watchDurable for a Storage with no push capability.
// It mirrors the push path's contract, including reading the current
// watermark BEFORE sleeping, so an already-satisfied wait costs nothing. It
// honours stop by aborting the sleep, so Server.Close does not wait out
// watchTimeout on these backends.
//
// The returned closure captures a timer and is not safe for concurrent use.
// That matches watchDurable's contract: run is its only caller.
func pollWatchDurable(store Storage, interval time.Duration) watchDurable {
	if interval <= 0 {
		interval = time.Millisecond
	}
	timer := time.NewTimer(interval)
	timer.Stop()
	return func(after uint64, stop <-chan struct{}) (uint64, error) {
		deadline := time.Now().Add(watchTimeout)
		for {
			durable, err := store.DurableSeq()
			if err != nil {
				return durable, err
			}
			if durable > after || !time.Now().Before(deadline) {
				return durable, nil
			}
			timer.Reset(interval)
			select {
			case <-stop:
				return durable, nil
			case <-timer.C:
			}
		}
	}
}
