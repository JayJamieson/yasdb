package ds

import (
	"sync"
	"sync/atomic"
)

// liveCache holds the most-recently committed record payloads in memory, so
// that caught-up live readers (long-poll/SSE) can assemble their next chunk
// without re-scanning the store. This is the fan-out read-amplification fix.
// Without it, a commit that appends one record to a stream with N parked
// readers triggers N independent store scans (per-record cgo/FFI,
// BENCHMARKS.md §4) for that one record. With it, the streamer caches the
// bytes it just wrote, and readers copy them out of RAM. A commit becomes
// one in-memory fan-out instead of N cgo re-scans. `-live-coalesce` bounds
// how *often* readers wake; this cache bounds what each wake *costs*.
//
// It is a bounded ring keyed by contiguous ascending seq. A reader whose
// start falls below the retained window (it fell too far behind, or just
// connected far back) misses, and falls back to readRange. Correctness is
// never at stake, only whether a given read touches the store. Only
// non-fork streams with live readers populate it (see
// streamer.applyReader); fork read-stitching stays on readRange.
type liveCache struct {
	mu       sync.RWMutex
	recs     []cachedRecord // contiguous, ascending seq
	curBytes int
	maxRecs  int
	maxBytes int

	hits atomic.Int64 // reads served from cache (store scans avoided)
}

type cachedRecord struct {
	seq     uint64
	payload []byte
}

// Default per-stream retained-window bounds (Config.LiveCacheMaxRecords /
// LiveCacheMaxBytes). These are sized for a caught-up reader, which only
// needs the last few commits, not a large backlog: about 256 KiB payload
// plus about 16 KiB of record headers per hot stream (a stream that is both
// written and has at least 1 live reader).
const (
	defaultLiveCacheMaxRecords = 512
	defaultLiveCacheMaxBytes   = 256 << 10 // 256 KiB
)

func newLiveCache(maxRecs, maxBytes int) *liveCache {
	return &liveCache{maxRecs: maxRecs, maxBytes: maxBytes}
}

// append adds a contiguous run of payloads starting at firstSeq (the
// reader-visible tail advance). A discontinuity (an unexpected firstSeq)
// drops the window and restarts it; readers then fall back to the store
// until it refills.
//
// It does NOT copy the payloads. Each is a slice into the freshly-allocated,
// write-once marshalRecord buffer the commit produced
// (recordPayload(op.Val)). The append has already crossed cgo into
// SlateDB's own memory, so the Go buffer is owned solely by this cache.
// Copying here would just duplicate the bytes marshalRecord already copied
// once.
func (c *liveCache) append(firstSeq uint64, payloads [][]byte) {
	if len(payloads) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := len(c.recs); n > 0 && firstSeq != c.recs[n-1].seq+1 {
		c.recs = c.recs[:0]
		c.curBytes = 0
	}
	for i, p := range payloads {
		c.recs = append(c.recs, cachedRecord{seq: firstSeq + uint64(i), payload: p})
		c.curBytes += len(p)
	}
	// Evict oldest records beyond the count/byte bounds, always keeping >= 1.
	drop := 0
	for len(c.recs)-drop > 1 && (len(c.recs)-drop > c.maxRecs || c.curBytes > c.maxBytes) {
		c.curBytes -= len(c.recs[drop].payload)
		drop++
	}
	if drop > 0 {
		c.recs = append(c.recs[:0], c.recs[drop:]...)
	}
}

// reset drops the retained window and frees its backing array. It is called
// when a stream has no live readers. A stream that keeps getting written
// with no readers hits this every commit, but after the first call it is a
// cheap no-op.
func (c *liveCache) reset() {
	c.mu.Lock()
	if c.recs != nil {
		c.recs = nil
		c.curBytes = 0
	}
	c.mu.Unlock()
}

// tryRead assembles the body for [start.Seq, tailSeq) from the cache into
// dst (reusing the caller's scratch buffer; pass nil for a fresh one). The
// result is byte-for-byte identical to readRange, or it returns ok=false to
// fall back to the store. It serves only the common caught-up case: whole
// records (start.Byte==0), a range fully covered by the retained window,
// and total bytes within the limit, so no capping or splitting is needed
// (readRange handles those cases).
func (c *liveCache) tryRead(dst []byte, isJSON bool, start Offset, tailSeq, limit uint64) (body []byte, next Offset, upToDate bool, ok bool) {
	if start.Byte != 0 || start.Seq >= tailSeq {
		return nil, start, false, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := len(c.recs)
	if n == 0 {
		return nil, start, false, false
	}
	lo := c.recs[0].seq
	hi := c.recs[n-1].seq + 1
	if start.Seq < lo || tailSeq > hi {
		return nil, start, false, false // range is not fully covered by the window
	}
	from := int(start.Seq - lo)
	to := int(tailSeq - lo)
	if from < 0 || to > n || from >= to {
		return nil, start, false, false
	}
	var total uint64
	for i := from; i < to; i++ {
		total += uint64(len(c.recs[i].payload))
	}
	// Serve only strictly under the limit. readRange stops *at* the limit,
	// and at an exact boundary it excludes trailing zero-length records
	// (its loop halts once the buffer reaches the cap). So `total == limit`
	// can diverge on the next offset. Falling back there, a rare
	// coincidence, keeps the two byte-identical.
	if total >= limit {
		return nil, start, false, false
	}
	dst = dst[:0]
	if isJSON {
		// Same bytes as appendJSONArray, straight from the cached records (no
		// intermediate [][]byte). from<to here, so the array is never empty.
		dst = append(dst, '[')
		for i := from; i < to; i++ {
			if i > from {
				dst = append(dst, ',')
			}
			dst = append(dst, c.recs[i].payload...)
		}
		dst = append(dst, ']')
	} else {
		for i := from; i < to; i++ {
			dst = append(dst, c.recs[i].payload...)
		}
	}
	c.hits.Add(1)
	return dst, Offset{Seq: tailSeq}, true, true
}
