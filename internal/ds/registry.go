package ds

import "sync"

// registryShardCount is the number of independent locks and maps the
// resident streamer registry splits across. A single global mutex here
// becomes a hard ceiling on registry throughput, at high
// concurrent-stream-count and high request-concurrency load, independent of
// core count. A pprof mutex profile at 32768 resident streams under 400
// concurrent VUs showed 100% of sampled contention tracing through this
// registry's lock (see BENCHMARKS.md). Sharding by a hash of the path lets
// up to this many operations proceed genuinely concurrently. Two requests
// only still contend if their streams happen to hash to the same shard.
const registryShardCount = 64

// streamerRegistry is the resident-streamer lookup table. It shards by a
// hash of the stream path, so no single mutex serialises every stream's
// registry operations. A given path always maps to the same shard (fnv1a is
// stable), so a stream's entry only ever lives in one shard. No cross-shard
// coordination is needed for any of the operations below.
type streamerRegistry struct {
	shards [registryShardCount]registryShard
}

// cacheLineSize is the padding target for registryShard. x86-64 and arm64
// both use 64-byte cache lines.
const cacheLineSize = 64

// registryShard is padded to its own cache line. Unpadded, it is a
// sync.Mutex (8 bytes) plus a map header (8 bytes), 16 bytes total. Four
// shards would fit on one cache line, so two goroutines locking two
// different, logically-independent shards could still contend by bouncing
// that shared line between cores (false sharing). This only bites because
// shards live inline in an array ([registryShardCount]registryShard in
// streamerRegistry, below), instead of behind separate heap allocations,
// which would have scattered them across lines for free.
type registryShard struct {
	mu        sync.Mutex
	streamers map[string]*streamer
	_         [cacheLineSize - 16]byte
}

func newStreamerRegistry() *streamerRegistry {
	r := &streamerRegistry{}
	for i := range r.shards {
		r.shards[i].streamers = make(map[string]*streamer)
	}
	return r
}

// fnv1a hashes path with zero allocation, unlike hash/fnv's hash.Hash
// interface. This runs on every registry operation, including the hot
// append path, so avoiding an allocation here matters.
func fnv1a(s string) uint32 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	h := uint32(offset32)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime32
	}
	return h
}

func (r *streamerRegistry) shardFor(path string) *registryShard {
	return &r.shards[fnv1a(path)%registryShardCount]
}

// get returns the resident streamer for path, if any.
func (r *streamerRegistry) get(path string) (*streamer, bool) {
	sh := r.shardFor(path)
	sh.mu.Lock()
	st, ok := sh.streamers[path]
	sh.mu.Unlock()
	return st, ok
}

// set registers st under path.
func (r *streamerRegistry) set(path string, st *streamer) {
	sh := r.shardFor(path)
	sh.mu.Lock()
	sh.streamers[path] = st
	sh.mu.Unlock()
}

// deleteIfCurrent removes path's entry only if it is still st. This guards
// against a respawned streamer being deleted by a stale unregister call. It
// is the same compare-before-delete the single-map version relied on.
func (r *streamerRegistry) deleteIfCurrent(path string, st *streamer) {
	sh := r.shardFor(path)
	sh.mu.Lock()
	if cur, ok := sh.streamers[path]; ok && cur == st {
		delete(sh.streamers, path)
	}
	sh.mu.Unlock()
}

// deleteAndReturn removes and returns whatever streamer is currently
// registered for path (nil if none).
func (r *streamerRegistry) deleteAndReturn(path string) *streamer {
	sh := r.shardFor(path)
	sh.mu.Lock()
	st := sh.streamers[path]
	if st != nil {
		delete(sh.streamers, path)
	}
	sh.mu.Unlock()
	return st
}

// drain empties every shard and returns everything that was resident.
// Close() uses this to snapshot, then stop, without holding any shard lock
// while it touches a streamer's own mutex. This preserves the st.mu ->
// shard.mu lock ordering tryRetire depends on (see Close's comment).
func (r *streamerRegistry) drain() []*streamer {
	var all []*streamer
	for i := range r.shards {
		sh := &r.shards[i]
		sh.mu.Lock()
		for _, st := range sh.streamers {
			all = append(all, st)
		}
		sh.streamers = make(map[string]*streamer)
		sh.mu.Unlock()
	}
	return all
}

// spawnLockShardCount shards the getOrSpawn-vs-delete serialization lock by
// path hash. It uses the same rationale and count as registryShardCount
// above: the old global metaMu serialized every stream's first-touch spawn
// and every delete server-wide, and a pprof mutex profile at 32768 resident
// streams found 99.85% of all sampled contention through that one lock.
// getOrSpawn never allocates a stream id or touches Server.nextID, so unlike
// a full sharding of metaMu, id allocation does not need to move. See
// removeStreamerLocked (expiry.go) for the delete-side half.
const spawnLockShardCount = 64

// spawnLock is padded to its own cache line, for the same false-sharing
// reason registryShard is (see its doc comment). A bare sync.Mutex is 8
// bytes, so 8 unpadded shards would fit on one 64-byte line.
type spawnLock struct {
	mu sync.Mutex
	_  [cacheLineSize - 8]byte
}

// spawnLocks is the sharded lock that getOrSpawn and removeStreamerLocked
// use in place of the old global metaMu, for spawn-vs-delete serialization.
// A given path always hashes to the same shard, so that stream's own race
// is always caught by the same lock. Two different streams' spawn/delete
// races never contend with each other, unless they collide on the same
// shard.
type spawnLocks struct {
	shards [spawnLockShardCount]spawnLock
}

func newSpawnLocks() *spawnLocks { return &spawnLocks{} }

func (l *spawnLocks) lockFor(path string) *sync.Mutex {
	return &l.shards[fnv1a(path)%spawnLockShardCount].mu
}
