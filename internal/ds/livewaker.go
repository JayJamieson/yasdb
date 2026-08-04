package ds

import (
	"sync"
	"time"
)

// liveWaker delivers commit wakeups to parked live readers. It can
// optionally coalesce a burst of commits into fewer wakeups.
//
// The v1 wake path closes a single shared broadcast channel on *every*
// commit (streamer.broadcast). With N parked readers, that is a thundering
// herd: all N goroutines become runnable at once, and each re-scans the
// store (a per-record cgo/FFI cost) for the newly committed records. Under
// a high commit rate with many readers, the herd cost O(commit_rate × N ×
// per_reader) outruns the cores, and the live-read path collapses (see
// BENCHMARKS.md "live fan-out").
//
// Coalescing bounds the wake rate to about 1/window, using leading-edge
// debounce:
//   - a commit after a quiet gap at or above window wakes readers
//     immediately (no added latency for low-rate streams), then
//   - commits inside the window fold into a single trailing wake, fired at
//     the window boundary.
//
// It never drops or reorders data: readers always re-read to the current
// s.tail.Load() when woken, and tail is published (applyReader) before Wake
// is called. So one wake after a burst delivers every accumulated record in
// order. A window of 0 preserves the exact v1 behavior (immediate wake per
// commit).
//
// Wake is safe to call from any goroutine. The streamer goroutine drives it
// in sync mode, and the durability-notifier callback goroutine drives it in
// notifier mode.
type liveWaker struct {
	s      *streamer
	window time.Duration

	mu       sync.Mutex
	lastWake time.Time
	timer    *time.Timer // armed while a trailing wake is deferred; else nil
	pending  bool
}

func newLiveWaker(s *streamer, window time.Duration) *liveWaker {
	return &liveWaker{s: s, window: window}
}

// Wake requests that parked readers be woken for a fresh commit, subject to
// coalescing.
func (w *liveWaker) Wake() {
	if w.window <= 0 {
		w.s.broadcast() // v1: immediate wake on every commit
		return
	}
	w.mu.Lock()
	now := time.Now()
	if now.Sub(w.lastWake) >= w.window {
		// Leading edge: quiet long enough, wake now.
		w.lastWake = now
		w.mu.Unlock()
		w.s.broadcast()
		return
	}
	// Inside the window of the last wake: fold into a single trailing wake.
	if w.pending {
		w.mu.Unlock()
		return
	}
	w.pending = true
	delay := w.window - now.Sub(w.lastWake)
	if delay < 0 {
		delay = 0
	}
	if w.timer == nil {
		w.timer = time.AfterFunc(delay, w.fire)
	} else {
		w.timer.Reset(delay)
	}
	w.mu.Unlock()
}

func (w *liveWaker) fire() {
	w.mu.Lock()
	w.pending = false
	w.lastWake = time.Now()
	w.mu.Unlock()
	w.s.broadcast()
}

// stop cancels any deferred trailing wake. The streamer calls this when it
// retires, so a timer does not linger holding the streamer alive. A stray
// fire after stop is harmless: it only swaps or closes an unobserved
// channel.
func (w *liveWaker) stop() {
	w.mu.Lock()
	if w.timer != nil {
		w.timer.Stop()
	}
	w.pending = false
	w.mu.Unlock()
}
