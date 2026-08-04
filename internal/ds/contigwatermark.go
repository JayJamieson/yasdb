package ds

import "sync"

// contigWatermark tracks the largest contiguous prefix [1..N] of
// wrapper-level sequence numbers marked complete. It folds out-of-order
// completions in as they arrive. pwalStore uses this: its shards flush
// independently and issue their own sequence numbers, so completions arrive
// out of order relative to each other. Naively taking min(shardWatermark)
// breaks the instant load across shards gets uneven.
type contigWatermark struct {
	mu    sync.Mutex
	set   map[uint64]struct{}
	value uint64
}

func newContigWatermark() *contigWatermark {
	return &contigWatermark{set: make(map[uint64]struct{})}
}

// mark folds newly-completed seqs into the watermark. seqs need not be
// sorted or contiguous. Any that do not immediately extend the prefix wait
// until the gap-filling seq arrives in a later call.
func (w *contigWatermark) mark(seqs []uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, sq := range seqs {
		w.set[sq] = struct{}{}
	}
	v := w.value
	for {
		if _, ok := w.set[v+1]; !ok {
			break
		}
		delete(w.set, v+1)
		v++
	}
	w.value = v
}

func (w *contigWatermark) get() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.value
}
