package ds

import "time"

// deleteStreamLocked hard-deletes a stream: it clears its addressable
// state, records a resumable deletion cursor, and starts background record
// deletion. When the stream is itself a fork, it also drops its
// source's reference count, cascading the source's cleanup if that empties
// it. The caller must hold metaMu. Explicit DELETE, expiry, and
// cascade GC all share this function.
func (s *Server) deleteStreamLocked(path string, meta *StreamMeta) error {
	id := meta.StreamID
	ops := []Op{
		{Key: metaKey(path), Del: true},
		{Key: idMappingKey(id), Del: true},
		{Key: tailKey(id), Del: true},
		{Key: writerSeqKey(id), Del: true},
		{Key: refCountKey(id), Del: true},
		{Key: tombstoneKey(path), Del: true}, // clear any soft-delete tombstone
		{Key: deletePendingKey(id), Val: u64Value(0)},
	}
	if meta.Deadline > 0 {
		ops = append(ops, Op{Key: expiryKey(meta.Deadline, id), Del: true})
	}
	if err := s.removeStreamerLocked(path, ops); err != nil {
		return err
	}
	s.startBackgroundDelete(id)

	// A fork releases its source, and cascades if the source is now
	// unreferenced and soft-deleted. This runs deliberately after
	// removeStreamerLocked has already released its spawn-lock shard: this
	// call can recurse into deleteStreamLocked for the source's own
	// (different) path, which needs its own shard. Nesting two acquisitions
	// on one goroutine risks a self-deadlock on a hash collision.
	if meta.ForkedFrom != 0 {
		s.dropForkRef(meta.ForkedFrom)
	}
	return nil
}

// softDeleteLocked marks a still-referenced stream soft-deleted. It retains
// all data for fork readers, but serves 410 on its own path. The
// caller holds metaMu.
func (s *Server) softDeleteLocked(path string, meta *StreamMeta) error {
	return s.removeStreamerLocked(path, []Op{
		{Key: tombstoneKey(path), Val: tombstoneValue(tombSoftDelete, meta.StreamID)},
	})
}

// removeStreamerLocked retires path's resident streamer, if any, and
// commits ops. It does this atomically with a concurrent getOrSpawn for
// path (spawnLocks, in registry.go). It is scoped to just this step, not
// the caller's whole delete, so a cascade to a different path never nests
// two spawnLocks acquisitions on the same goroutine.
func (s *Server) removeStreamerLocked(path string, ops []Op) error {
	mu := s.spawnLocks.lockFor(path)
	mu.Lock()
	defer mu.Unlock()
	s.removeStreamer(path)
	return s.store.Commit(ops, true)
}

// dropForkRef decrements a source's reference count. When the count reaches
// zero and the source is soft-deleted, it hard-deletes the source too,
// cascading up the chain. The caller holds metaMu.
func (s *Server) dropForkRef(sourceID uint64) {
	count := s.loadRefCount(sourceID)
	if count > 0 {
		count--
	}
	_ = s.store.Commit([]Op{{Key: refCountKey(sourceID), Val: u64Value(count)}}, true)
	if count > 0 {
		return
	}
	meta, srcPath, ok, err := s.loadMetaByID(sourceID)
	if err != nil || !ok {
		return
	}
	if _, _, found, _ := s.loadTombstone(srcPath); found {
		_ = s.deleteStreamLocked(srcPath, meta) // recurses / cascades
	}
}

// tombstoneValue encodes a PathTombstone value: reason:u8, id:u64.
func tombstoneValue(reason byte, id uint64) []byte {
	return append([]byte{reason}, be64(id)...)
}

// runSweeper periodically expires streams whose TTL or Stream-Expires-At
// deadline has passed. The deadline index is keyed deadline-first, so it
// finds all due entries with a single bounded range scan.
func (s *Server) runSweeper() {
	defer s.bgwg.Done()
	t := time.NewTicker(s.cfg.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-s.sweepStop:
			return
		case <-t.C:
			s.sweepOnce(time.Now().Unix())
		}
	}
}

func (s *Server) sweepOnce(now int64) {
	it, err := s.store.Scan([]byte{byte(ktExpiryDeadline)}, expiryScanEnd(uint64(now)))
	if err != nil {
		return
	}
	type due struct {
		deadline, id uint64
		key          []byte
	}
	var dues []due
	for {
		k, _, ok, err := it.Next()
		if err != nil || !ok {
			break
		}
		if len(k) >= 17 {
			kc := make([]byte, len(k))
			copy(kc, k)
			dues = append(dues, due{deadline: be64Decode(k[1:9]), id: be64Decode(k[9:17]), key: kc})
		}
	}
	it.Close()

	for _, d := range dues {
		s.expireIfDue(d.deadline, d.id, d.key, now)
	}
}

// expireIfDue removes one due deadline entry. It expires the stream when
// the entry is still the current deadline. It simply cleans up stale
// entries: the stream is gone, or a refresh moved the deadline forward.
func (s *Server) expireIfDue(deadline, id uint64, key []byte, now int64) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()

	pathB, found, err := s.store.Get(idMappingKey(id))
	if err != nil {
		return
	}
	if !found {
		s.dropStaleExpiry(key)
		return
	}
	meta, ok, err := s.loadMeta(string(pathB))
	if err != nil {
		return
	}
	if !ok || meta.StreamID != id || meta.Deadline != deadline {
		s.dropStaleExpiry(key)
		return
	}
	if int64(deadline) > now {
		return // refreshed to the future between scan and now
	}
	_ = s.deleteStreamLocked(string(pathB), meta)
}

func (s *Server) dropStaleExpiry(key []byte) {
	_ = s.store.Commit([]Op{{Key: key, Del: true}}, false)
}
