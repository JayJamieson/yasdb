package ds

import (
	"mime"
	"strings"
	"sync"
	"time"
)

// Header names (protocol §13.2).
const (
	hContentType         = "Content-Type"
	hCacheControl        = "Cache-Control"
	hETag                = "ETag"
	hIfNoneMatch         = "If-None-Match"
	hLocation            = "Location"
	hStreamTTL           = "Stream-TTL"
	hStreamExpires       = "Stream-Expires-At"
	hStreamSeq           = "Stream-Seq"
	hStreamCursor        = "Stream-Cursor"
	hStreamNext          = "Stream-Next-Offset"
	hStreamUpToDate      = "Stream-Up-To-Date"
	hStreamClosed        = "Stream-Closed"
	hForkedFrom          = "Stream-Forked-From"
	hForkOffset          = "Stream-Fork-Offset"
	hForkSubOffset       = "Stream-Fork-Sub-Offset"
	hProducerID          = "Producer-Id"
	hProducerEpoch       = "Producer-Epoch"
	hProducerSeq         = "Producer-Seq"
	hProducerExpectedSeq = "Producer-Expected-Seq"
	hProducerReceivedSeq = "Producer-Received-Seq"
	hSSEDataEncoding     = "stream-sse-data-encoding"
)

const defaultContentType = "application/octet-stream"

// PathTombstone reasons.
const (
	tombHardDelete byte = 1
	tombSoftDelete byte = 2
)

// Config configures a Server.
type Config struct {
	MaxRecordBytes  int64
	LongPollTimeout time.Duration
	SSELifetime     time.Duration
	DormancyTimeout time.Duration
	SweepInterval   time.Duration // how often the expiry sweeper runs
	MaxReadBytes    uint64        // soft cap on bytes returned per read

	// LiveCoalesceWindow bounds how often parked live readers (long-poll/SSE) are
	// woken for new commits. 0 (default) wakes on every commit — the original
	// behavior. A small positive value (e.g. 2–5ms) folds a burst of commits into
	// one wake via leading-edge debounce, taming the fan-out thundering herd under
	// high commit-rate × many readers (see livewaker.go, BENCHMARKS.md). Adds at
	// most this much latency to live delivery for hot streams; none for quiet ones.
	LiveCoalesceWindow time.Duration

	// DisableLiveCache turns off the in-memory recent-records cache that lets
	// caught-up live readers assemble their next chunk without a store scan
	// (livecache.go). Default (false) keeps it on. An escape hatch / benchmarking
	// knob; correctness is identical either way (the cache falls back to readRange).
	DisableLiveCache bool

	// LiveCacheMaxRecords / LiveCacheMaxBytes bound the per-stream cache retained
	// window (whichever binds first). 0 uses the defaults (512 records / 256 KiB).
	// A caught-up reader only needs the last few commits, so this is small; a
	// reader lagging beyond the window falls back to a store scan for that read.
	// Lower to cut memory on many-hot-stream deployments; raise to serve laggier
	// readers from RAM.
	LiveCacheMaxRecords int
	LiveCacheMaxBytes   int

	// Durability selects how appends are acknowledged:
	//   "" / "sync"   — block each group-commit on a durable write (default).
	//   "notifier"    — write non-durably and ack via the durable-seq notifier;
	//                   appends pipeline instead of blocking on the flush.
	// Both ack Stream-Next-Offset only once the data is durable.
	Durability           string
	NotifierPollInterval time.Duration // durable-seq poll cadence (notifier mode)
}

// Server implements the Durable Streams HTTP surface over a Storage backend.
type Server struct {
	store Storage
	cfg   Config

	regMu     sync.Mutex
	streamers map[string]*streamer

	metaMu sync.Mutex
	nextID uint64

	committer committer      // durability strategy (sync / notifier)
	sweepStop chan struct{}  // closed on Close to stop the sweeper
	bgwg      sync.WaitGroup // tracks the sweeper + background delete goroutines
}

// NewServer builds a Server over store, recovering the id counter and resuming
// any interrupted deletions.
func NewServer(store Storage, cfg Config) (*Server, error) {
	if cfg.MaxRecordBytes == 0 {
		cfg.MaxRecordBytes = 4 << 20
	}
	if cfg.LongPollTimeout == 0 {
		cfg.LongPollTimeout = 25 * time.Second
	}
	if cfg.SSELifetime == 0 {
		cfg.SSELifetime = 60 * time.Second
	}
	if cfg.DormancyTimeout == 0 {
		cfg.DormancyTimeout = dormancyTimeout
	}
	if cfg.SweepInterval == 0 {
		cfg.SweepInterval = time.Second
	}
	if cfg.MaxReadBytes == 0 {
		cfg.MaxReadBytes = readChunkBytes
	}
	if cfg.LiveCacheMaxRecords == 0 {
		cfg.LiveCacheMaxRecords = defaultLiveCacheMaxRecords
	}
	if cfg.LiveCacheMaxBytes == 0 {
		cfg.LiveCacheMaxBytes = defaultLiveCacheMaxBytes
	}
	s := &Server{
		store:     store,
		cfg:       cfg,
		streamers: make(map[string]*streamer),
		nextID:    1,
		sweepStop: make(chan struct{}),
	}
	if v, found, err := store.Get(idCounterKey()); err != nil {
		return nil, err
	} else if found {
		if id, err := parseU64Value(v); err == nil && id >= 1 {
			s.nextID = id
		}
	}
	s.committer = newCommitter(store, cfg)
	s.resumeDeletions()
	s.bgwg.Add(1)
	go s.runSweeper()
	return s, nil
}

// Close shuts down resident streamers and the store. Callers must stop accepting
// requests first (main.go drains the HTTP server before calling Close): freeing
// the native store while an FFI call is in flight is a use-after-free.
func (s *Server) Close() error {
	close(s.sweepStop)
	_ = s.committer.close()
	// Join the sweeper and any in-flight background deletions before freeing the
	// store, so a range-delete scan can never hit an already-destroyed handle.
	s.bgwg.Wait()

	// Snapshot and clear the registry under regMu, then stop each streamer WITHOUT
	// holding regMu. Locking st.mu while holding regMu would invert the lock order
	// used by tryRetire (st.mu -> regMu via unregister) and could deadlock.
	s.regMu.Lock()
	sts := make([]*streamer, 0, len(s.streamers))
	for _, st := range s.streamers {
		sts = append(sts, st)
	}
	s.streamers = make(map[string]*streamer)
	s.regMu.Unlock()

	for _, st := range sts {
		st.mu.Lock()
		if !st.dead {
			st.dead = true
			close(st.stop)
		}
		st.mu.Unlock()
	}
	return s.store.Close()
}

// --- metadata loads ---

func (s *Server) loadMeta(path string) (*StreamMeta, bool, error) {
	v, found, err := s.store.Get(metaKey(path))
	if err != nil || !found {
		return nil, false, err
	}
	m, err := unmarshalMeta(v)
	if err != nil {
		return nil, false, err
	}
	return m, true, nil
}

func (s *Server) loadTombstone(path string) (reason byte, id uint64, found bool, err error) {
	v, ok, err := s.store.Get(tombstoneKey(path))
	if err != nil || !ok {
		return 0, 0, false, err
	}
	if len(v) < 9 {
		return 0, 0, false, errBadMeta
	}
	return v[0], be64Decode(v[1:9]), true, nil
}

func (s *Server) loadTailSeq(id uint64) (uint64, error) {
	v, found, err := s.store.Get(tailKey(id))
	if err != nil || !found {
		return 0, err
	}
	seq, _, err := unmarshalTail(v)
	return seq, err
}

// --- streamer registry ---

// getOrSpawn returns the resident streamer for path, spawning one from durable
// state if needed. found is false when the stream does not exist.
//
// The spawn path is serialised with create/delete via metaMu so a streamer can
// never be registered from meta that a concurrent DELETE has already removed
// (which would otherwise leave a "zombie" streamer serving ghost records during
// background deletion). Once resident, appends and live reads hit only the
// lock-free fast path below.
func (s *Server) getOrSpawn(path string) (*streamer, bool, error) {
	s.regMu.Lock()
	if st, ok := s.streamers[path]; ok {
		s.regMu.Unlock()
		return st, true, nil
	}
	s.regMu.Unlock()

	s.metaMu.Lock()
	defer s.metaMu.Unlock()

	// Another goroutine may have spawned it while we waited for metaMu.
	s.regMu.Lock()
	if st, ok := s.streamers[path]; ok {
		s.regMu.Unlock()
		return st, true, nil
	}
	s.regMu.Unlock()

	// metaMu is held, so DELETE cannot interleave: either it already removed the
	// stream (loadMeta returns not-found here) or it runs strictly after we
	// register, in which case removeStreamer will retire this streamer.
	meta, ok, err := s.loadMeta(path)
	if err != nil || !ok {
		return nil, false, err
	}
	tailSeq, err := s.loadTailSeq(meta.StreamID)
	if err != nil {
		return nil, false, err
	}

	st := newStreamer(s, path, meta, tailSeq)
	s.regMu.Lock()
	s.streamers[path] = st
	s.regMu.Unlock()
	go st.run()
	return st, true, nil
}

// touchStream signals the owning streamer to slide the sliding-TTL window for a
// read. Non-blocking and best-effort; coalesces via the buffered touch channel.
func (s *Server) touchStream(path string) {
	st, found, err := s.getOrSpawn(path)
	if err != nil || !found {
		return
	}
	st.mu.Lock()
	if !st.dead {
		select {
		case st.touch <- struct{}{}:
		default:
		}
	}
	st.mu.Unlock()
}

// unregister removes st from the registry if it is still the current entry.
func (s *Server) unregister(st *streamer) {
	s.regMu.Lock()
	if cur, ok := s.streamers[st.path]; ok && cur == st {
		delete(s.streamers, st.path)
	}
	s.regMu.Unlock()
}

// removeStreamer forcibly retires the streamer for path (used by delete). Any
// buffered appends are answered 404 by the streamer's stop drain.
func (s *Server) removeStreamer(path string) {
	s.regMu.Lock()
	st := s.streamers[path]
	if st != nil {
		delete(s.streamers, path)
	}
	s.regMu.Unlock()
	if st == nil {
		return
	}
	st.mu.Lock()
	if !st.dead {
		st.dead = true
		close(st.stop)
	}
	st.mu.Unlock()
}

// submitAppend routes an append to the owning streamer, retrying if the streamer
// retired between lookup and enqueue. found is false when the stream is gone.
func (s *Server) submitAppend(path string, req appendReq) (appendResp, bool) {
	for {
		st, found, err := s.getOrSpawn(path)
		if err != nil {
			return appendResp{status: 500, err: err}, true
		}
		if !found {
			return appendResp{}, false
		}
		st.mu.Lock()
		if st.dead {
			st.mu.Unlock()
			continue
		}
		req.resp = make(chan appendResp, 1)
		st.reqs <- req
		st.mu.Unlock()
		return <-req.resp, true
	}
}

// liveState returns the current tail and closed status for a stream, preferring a
// resident streamer's in-memory view and falling back to durable state. The
// returned streamer is nil when the value came from durable state (no resident
// streamer), so callers can distinguish live from loaded.
func (s *Server) liveState(path string) (tail uint64, closed bool, st *streamer, found bool, err error) {
	s.regMu.Lock()
	resident := s.streamers[path]
	s.regMu.Unlock()
	if resident != nil {
		return resident.tail.Load(), resident.closed.Load(), resident, true, nil
	}
	meta, ok, err := s.loadMeta(path)
	if err != nil || !ok {
		return 0, false, nil, false, err
	}
	tailSeq, err := s.loadTailSeq(meta.StreamID)
	if err != nil {
		return 0, false, nil, false, err
	}
	return tailSeq, meta.Closed, nil, true, nil
}

// --- helpers ---

func be64Decode(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// mediaType normalises a content-type to its lowercase base media type.
func mediaType(ct string) string {
	ct = strings.TrimSpace(ct)
	if ct == "" {
		return ""
	}
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		return mt
	}
	return strings.ToLower(ct)
}

func contentTypeMatches(configured, req string) bool {
	return mediaType(configured) == mediaType(req)
}

func isJSONStream(ct string) bool { return mediaType(ct) == contentTypeJSON }

// isTextLike reports whether SSE can carry the content type as UTF-8 text
// (protocol §5.8); everything else is base64-encoded.
func isTextLike(ct string) bool {
	mt := mediaType(ct)
	return mt == contentTypeJSON || strings.HasPrefix(mt, "text/")
}
