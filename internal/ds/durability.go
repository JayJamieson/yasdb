package ds

import (
	"sort"
	"sync"
	"time"
)

// durabilityNotifier acks writes once the engine's durable watermark reaches
// their assigned sequence number, instead of blocking each write on an fsync
// (SPEC §6 "durability notifier"; the design s2-lite uses). One poller serves
// every stream: appends never block on durability, so a single stream pipelines
// many appends into one flush. Used when Config.Durability == "notifier".
//
// The Go SlateDB binding exposes the durable watermark by poll (Db.Status()),
// not a subscription, so this polls at a small interval; it only polls while
// waiters are outstanding.
type durabilityNotifier struct {
	store    Storage
	interval time.Duration

	mu          sync.Mutex
	waiters     []durWaiter // sorted ascending by target
	lastDurable uint64
	closed      bool

	wake chan struct{}
	stop chan struct{}
}

type durWaiter struct {
	target uint64
	cb     func(error)
}

func newDurabilityNotifier(store Storage, interval time.Duration) *durabilityNotifier {
	if interval <= 0 {
		interval = time.Millisecond
	}
	n := &durabilityNotifier{
		store:    store,
		interval: interval,
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
	}
	go n.run()
	return n
}

// subscribe invokes cb exactly once: with nil when the durable watermark reaches
// target, or with an error if the store is (or becomes) closed.
func (n *durabilityNotifier) subscribe(target uint64, cb func(error)) {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		cb(errStoreClosed)
		return
	}
	if target <= n.lastDurable {
		n.mu.Unlock()
		cb(nil)
		return
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
}

func (n *durabilityNotifier) run() {
	t := time.NewTicker(n.interval)
	defer t.Stop()
	for {
		select {
		case <-n.stop:
			n.closeAll(errStoreClosed)
			return
		case <-n.wake:
			n.poll()
		case <-t.C:
			n.poll()
		}
	}
}

func (n *durabilityNotifier) poll() {
	n.mu.Lock()
	empty := len(n.waiters) == 0
	n.mu.Unlock()
	if empty {
		return
	}
	seq, err := n.store.DurableSeq()
	if err != nil {
		n.closeAll(err)
		return
	}
	n.fire(seq)
}

func (n *durabilityNotifier) fire(durable uint64) {
	n.mu.Lock()
	if durable > n.lastDurable {
		n.lastDurable = durable
	}
	i := 0
	for i < len(n.waiters) && n.waiters[i].target <= durable {
		i++
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

func (n *durabilityNotifier) shutdown() {
	select {
	case <-n.stop:
	default:
		close(n.stop)
	}
}
