package ds

import (
	"log"
	"strings"
)

// isShutdownErr reports whether err is just the store being closed (server
// shutdown / test teardown), which is not worth logging.
func isShutdownErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "closed")
}

// deleteChunk is how many records are range-deleted per durable batch.
const deleteChunk = 1000

// resumeDeletions restarts any deletions that were interrupted by a crash. The
// DeletePending index makes deletion resumable rather than restartable (SPEC §10).
func (s *Server) resumeDeletions() {
	it, err := s.store.Scan([]byte{byte(ktDeletePending)}, prefixEnd([]byte{byte(ktDeletePending)}))
	if err != nil {
		return
	}
	var ids []uint64
	for {
		k, _, ok, err := it.Next()
		if err != nil || !ok {
			break
		}
		if len(k) >= 9 {
			ids = append(ids, be64Decode(k[1:9]))
		}
	}
	it.Close()
	for _, id := range ids {
		s.startBackgroundDelete(id)
	}
}

// startBackgroundDelete runs a resumable deletion under the background WaitGroup
// so Close can join it before freeing the store.
func (s *Server) startBackgroundDelete(id uint64) {
	s.bgwg.Add(1)
	go func() {
		defer s.bgwg.Done()
		s.backgroundDelete(id)
	}()
}

// backgroundDelete range-deletes a stream's records and producer state in
// bounded, resumable chunks, then clears the DeletePending marker.
func (s *Server) backgroundDelete(id uint64) {
	cursor := uint64(0)
	if v, found, err := s.store.Get(deletePendingKey(id)); err == nil && found {
		if c, err := parseU64Value(v); err == nil {
			cursor = c
		}
	}

	for {
		keys, last, err := s.collectKeys(recordKey(id, cursor), recordPrefixEnd(id), deleteChunk)
		if err != nil {
			if !isShutdownErr(err) {
				log.Printf("yasdb: delete stream %d: scan error: %v", id, err)
			}
			return
		}
		if len(keys) == 0 {
			break
		}
		ops := make([]Op, 0, len(keys)+1)
		for _, k := range keys {
			ops = append(ops, Op{Key: k, Del: true})
		}
		cursor = last + 1
		ops = append(ops, Op{Key: deletePendingKey(id), Val: u64Value(cursor)})
		if err := s.store.Commit(ops, true); err != nil {
			if !isShutdownErr(err) {
				log.Printf("yasdb: delete stream %d: commit error: %v", id, err)
			}
			return
		}
	}

	// Reclaim producer state.
	for {
		keys, _, err := s.collectKeys(producerPrefix(id), producerPrefixEnd(id), deleteChunk)
		if err != nil || len(keys) == 0 {
			break
		}
		ops := make([]Op, len(keys))
		for i, k := range keys {
			ops[i] = Op{Key: k, Del: true}
		}
		if err := s.store.Commit(ops, true); err != nil {
			if !isShutdownErr(err) {
				log.Printf("yasdb: delete stream %d: producer cleanup error: %v", id, err)
			}
			return
		}
	}

	if err := s.store.Commit([]Op{{Key: deletePendingKey(id), Del: true}}, true); err != nil {
		if !isShutdownErr(err) {
			log.Printf("yasdb: delete stream %d: clear pending error: %v", id, err)
		}
	}
}

// collectKeys returns up to limit keys in [start, end) plus the seq of the last
// record key seen (only meaningful for record scans).
func (s *Server) collectKeys(start, end []byte, limit int) (keys [][]byte, lastSeq uint64, err error) {
	it, err := s.store.Scan(start, end)
	if err != nil {
		return nil, 0, err
	}
	defer it.Close()
	for len(keys) < limit {
		k, _, ok, err := it.Next()
		if err != nil {
			return nil, 0, err
		}
		if !ok {
			break
		}
		kc := make([]byte, len(k))
		copy(kc, k)
		keys = append(keys, kc)
		if len(k) >= 17 {
			lastSeq = be64Decode(k[9:17])
		}
	}
	return keys, lastSeq, nil
}
