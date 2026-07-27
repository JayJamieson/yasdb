package ds

import "encoding/binary"

// KeyType is the leading ordinal byte of every key in the shared keyspace.
//
// These numeric values are PERSISTED and MUST NEVER be reused. See SPEC.md §3.
// All multi-byte integers in keys are big-endian so that SlateDB's byte-wise
// ordering matches numeric ordering.
type KeyType byte

const (
	ktStreamMeta      KeyType = 1 // path                -> StreamMeta
	ktStreamIDMapping KeyType = 2 // id:u64              -> path
	ktStreamTail      KeyType = 3 // id:u64              -> seq:u64, updated:i64
	ktRecordData      KeyType = 4 // id:u64, seq:u64     -> flags:u8, data
	ktProducerState   KeyType = 5 // id:u64, producerID  -> epoch, lastSeq, closedBy
	ktWriterSeq       KeyType = 6 // id:u64              -> last Stream-Seq string
	ktExpiryDeadline  KeyType = 7 // deadline:u64, id:u64 -> ttlSecs:u64
	ktDeletePending   KeyType = 8 // id:u64              -> cursorSeq:u64
	ktPathTombstone   KeyType = 9 // path                -> reason:u8, id:u64

	// ktRefCount holds the number of forks referencing a stream. It is kept out
	// of StreamMeta so create/delete can mutate it under metaMu without racing
	// the streamer's meta writes.
	ktRefCount KeyType = 10 // id:u64 -> count:u64

	// ktIDCounter is a singleton key holding the next StreamID to allocate.
	// It lives in its own ordinal, added by this implementation (see SPEC §3:
	// StreamID "is allocated from a monotonic counter persisted ... (or a
	// dedicated singleton key)").
	ktIDCounter KeyType = 100
)

func be64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func metaKey(path string) []byte {
	return append([]byte{byte(ktStreamMeta)}, path...)
}

func idMappingKey(id uint64) []byte {
	return append([]byte{byte(ktStreamIDMapping)}, be64(id)...)
}

func tailKey(id uint64) []byte {
	return append([]byte{byte(ktStreamTail)}, be64(id)...)
}

func recordKey(id, seq uint64) []byte {
	k := make([]byte, 0, 17)
	k = append(k, byte(ktRecordData))
	k = append(k, be64(id)...)
	k = append(k, be64(seq)...)
	return k
}

func recordPrefix(id uint64) []byte {
	return append([]byte{byte(ktRecordData)}, be64(id)...)
}

// recordPrefixEnd returns the exclusive end key for scanning all records of id.
func recordPrefixEnd(id uint64) []byte {
	return prefixEnd(recordPrefix(id))
}

func producerKey(id uint64, producerID string) []byte {
	k := make([]byte, 0, 9+len(producerID))
	k = append(k, byte(ktProducerState))
	k = append(k, be64(id)...)
	k = append(k, producerID...)
	return k
}

func producerPrefix(id uint64) []byte {
	return append([]byte{byte(ktProducerState)}, be64(id)...)
}

func producerPrefixEnd(id uint64) []byte {
	return prefixEnd(producerPrefix(id))
}

func writerSeqKey(id uint64) []byte {
	return append([]byte{byte(ktWriterSeq)}, be64(id)...)
}

func expiryKey(deadline, id uint64) []byte {
	k := make([]byte, 0, 17)
	k = append(k, byte(ktExpiryDeadline))
	k = append(k, be64(deadline)...)
	k = append(k, be64(id)...)
	return k
}

// expiryScanEnd returns the exclusive end key covering all deadlines <= now.
func expiryScanEnd(now uint64) []byte {
	// deadlines are the first 8 bytes; end just past `now` so that any (now, id)
	// entry is included.
	return append([]byte{byte(ktExpiryDeadline)}, be64(now+1)...)
}

func deletePendingKey(id uint64) []byte {
	return append([]byte{byte(ktDeletePending)}, be64(id)...)
}

func tombstoneKey(path string) []byte {
	return append([]byte{byte(ktPathTombstone)}, path...)
}

func refCountKey(id uint64) []byte {
	return append([]byte{byte(ktRefCount)}, be64(id)...)
}

func idCounterKey() []byte {
	return []byte{byte(ktIDCounter)}
}

// prefixEnd returns the smallest byte string strictly greater than every string
// that has p as a prefix. It is the exclusive upper bound for a prefix scan.
func prefixEnd(p []byte) []byte {
	end := make([]byte, len(p))
	copy(end, p)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] != 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	// p was all 0xff bytes: no finite upper bound. nil means "to end" for scans.
	return nil
}
