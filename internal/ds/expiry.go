package ds

import "time"

// deleteStreamLocked hard-deletes a stream: clears its addressable state, records
// a resumable deletion cursor, starts background record deletion (SPEC §10), and
// — when the stream is itself a fork — drops its source's reference count,
// cascading the source's cleanup if that empties it (SPEC §11). The caller must
// hold metaMu. Shared by explicit DELETE, expiry, and cascade GC.
func (s *Server) deleteStreamLocked(path string, meta *StreamMeta) error {
	id := meta.StreamID
	s.removeStreamer(path) // no further appends can land

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
	if err := s.store.Commit(ops, true); err != nil {
		return err
	}
	s.startBackgroundDelete(id)

	// A fork releases its source; cascade if the source is now unreferenced and
	// soft-deleted.
	if meta.ForkedFrom != 0 {
		s.dropForkRef(meta.ForkedFrom)
	}
	return nil
}

// softDeleteLocked marks a still-referenced stream soft-deleted: it retains all
// data for fork readers but serves 410 on its own path (SPEC §11). Caller holds
// metaMu.
func (s *Server) softDeleteLocked(path string, meta *StreamMeta) error {
	s.removeStreamer(path)
	return s.store.Commit([]Op{
		{Key: tombstoneKey(path), Val: tombstoneValue(tombSoftDelete, meta.StreamID)},
	}, true)
}

// dropForkRef decrements a source's reference count and, when it reaches zero and
// the source is soft-deleted, hard-deletes it (cascading up the chain). Caller
// holds metaMu.
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

// runSweeper periodically expires streams whose TTL / Stream-Expires-At deadline
// has passed. The deadline index is keyed by deadline-first so all due entries
// are found with a single bounded range scan (SPEC §10).
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

// expireIfDue removes one due deadline entry, expiring the stream when the entry
// is still the current deadline. Stale entries (stream gone, or a refresh moved
// the deadline forward) are simply cleaned up.
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
