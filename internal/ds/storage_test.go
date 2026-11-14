package ds

import (
	"testing"
	"time"
)

// TestSlateStoreCloseWithWatchParked pins the two FFI facts slateStore.Close
// is ordered around. db.Shutdown() is the only call that unparks an in-flight
// WaitForChange, so a Close that destroyed the subscription first would leave
// the watcher parked until watchTimeout instead of returning at once. And a
// WatchDurable that starts after Close must report a closed store rather than
// panicking on a destroyed handle.
func TestSlateStoreCloseWithWatchParked(t *testing.T) {
	store, err := OpenStore(uniqueDBPath("closewatch"), "memory:///", StoreTuning{FlushInterval: time.Second})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	sub, ok := store.(DurabilitySubscriber)
	if !ok {
		t.Fatal("the SlateDB store must implement DurabilitySubscriber")
	}

	// 1<<40 is a watermark this store will never reach, so the call parks
	// until either Close or watchTimeout ends it.
	parked := make(chan error, 1)
	go func() {
		// slateStore ignores stop. What this pins is what Close does to a
		// watch already in flight.
		_, werr := sub.WatchDurable(1<<40, nil)
		parked <- werr
	}()
	time.Sleep(20 * time.Millisecond)

	start := time.Now()
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case werr := <-parked:
		elapsed := time.Since(start)
		t.Logf("parked watch returned %v, %v after Close", werr, elapsed)
		if werr == nil {
			t.Error("a watch unparked by Close must report the store closed")
		}
		if elapsed > watchTimeout/2 {
			t.Errorf("Close took %v to unpark the watch; it timed out rather than being woken", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch still parked 5s after Close")
	}

	if _, err := sub.WatchDurable(0, nil); err == nil {
		t.Error("WatchDurable after Close must return an error")
	}
}
