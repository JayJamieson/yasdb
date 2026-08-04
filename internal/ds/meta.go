package ds

import (
	"encoding/binary"
	"errors"
)

// StreamMeta is the mutable per-stream metadata stored under ktStreamMeta.
// It is encoded with a stable hand-rolled binary format, never encoding/gob.
type StreamMeta struct {
	StreamID    uint64
	ContentType string
	TTLSeconds  uint64 // 0 = none
	ExpiresAt   int64  // unix seconds; 0 = none; mutually exclusive with TTL
	Closed      bool   // monotonic; never cleared
	CreatedAtMs int64
	IsPrivate   bool   // drives Cache-Control public vs private
	Deadline    uint64 // unix seconds of the current expiry deadline in the index; 0 = none

	// These fork fields are immutable after creation, so they are safe to
	// read off the streamer goroutine. ForkedFrom is the source stream's id.
	// The divergence point is the offset (ForkOffset seq, ForkByte). Live
	// RefCount and soft-delete state live outside meta, in a dedicated
	// ktRefCount key and the PathTombstone, so their mutations never race
	// the streamer's meta writes.
	ForkedFrom uint64 // 0 = not a fork
	ForkOffset uint64 // source seq at the divergence point
	ForkByte   uint64 // byte within ForkOffset's record (binary sub-offset); 0 = boundary
}

const streamMetaVersion = 3

const (
	metaFlagClosed = 1 << iota
	metaFlagPrivate
	metaFlagSoftDeleted
)

// Marshal encodes the metadata.
func (m *StreamMeta) Marshal() []byte {
	buf := make([]byte, 0, 64+len(m.ContentType))
	buf = append(buf, streamMetaVersion)

	var flags byte
	if m.Closed {
		flags |= metaFlagClosed
	}
	if m.IsPrivate {
		flags |= metaFlagPrivate
	}
	buf = append(buf, flags)

	buf = appendU64(buf, m.StreamID)
	buf = appendU64(buf, m.TTLSeconds)
	buf = appendU64(buf, uint64(m.ExpiresAt))
	buf = appendU64(buf, uint64(m.CreatedAtMs))
	buf = appendU64(buf, m.ForkedFrom)
	buf = appendU64(buf, m.ForkOffset)
	buf = appendU64(buf, m.ForkByte)
	buf = appendU64(buf, m.Deadline)

	buf = appendU16(buf, uint16(len(m.ContentType)))
	buf = append(buf, m.ContentType...)
	return buf
}

var errBadMeta = errors.New("corrupt StreamMeta")

// unmarshalMeta decodes metadata produced by Marshal.
func unmarshalMeta(b []byte) (*StreamMeta, error) {
	if len(b) < 2 || b[0] != streamMetaVersion {
		return nil, errBadMeta
	}
	flags := b[1]
	p := 2
	rdU64 := func() (uint64, bool) {
		if p+8 > len(b) {
			return 0, false
		}
		v := binary.BigEndian.Uint64(b[p:])
		p += 8
		return v, true
	}

	var ok bool
	m := &StreamMeta{}
	m.Closed = flags&metaFlagClosed != 0
	m.IsPrivate = flags&metaFlagPrivate != 0

	if m.StreamID, ok = rdU64(); !ok {
		return nil, errBadMeta
	}
	if m.TTLSeconds, ok = rdU64(); !ok {
		return nil, errBadMeta
	}
	var u uint64
	if u, ok = rdU64(); !ok {
		return nil, errBadMeta
	}
	m.ExpiresAt = int64(u)
	if u, ok = rdU64(); !ok {
		return nil, errBadMeta
	}
	m.CreatedAtMs = int64(u)
	if m.ForkedFrom, ok = rdU64(); !ok {
		return nil, errBadMeta
	}
	if m.ForkOffset, ok = rdU64(); !ok {
		return nil, errBadMeta
	}
	if m.ForkByte, ok = rdU64(); !ok {
		return nil, errBadMeta
	}
	if m.Deadline, ok = rdU64(); !ok {
		return nil, errBadMeta
	}

	if p+2 > len(b) {
		return nil, errBadMeta
	}
	ctLen := int(binary.BigEndian.Uint16(b[p:]))
	p += 2
	if p+ctLen > len(b) {
		return nil, errBadMeta
	}
	m.ContentType = string(b[p : p+ctLen])
	return m, nil
}

// --- StreamTail value: seq:u64, updated:i64 ---

func marshalTail(seq uint64, updated int64) []byte {
	b := make([]byte, 16)
	binary.BigEndian.PutUint64(b[0:], seq)
	binary.BigEndian.PutUint64(b[8:], uint64(updated))
	return b
}

func unmarshalTail(b []byte) (seq uint64, updated int64, err error) {
	if len(b) < 16 {
		return 0, 0, errBadMeta
	}
	seq = binary.BigEndian.Uint64(b[0:])
	updated = int64(binary.BigEndian.Uint64(b[8:]))
	return seq, updated, nil
}

// --- ProducerState value: epoch:u64, lastSeq:u64, closedBy:bool ---

type producerState struct {
	epoch    uint64
	lastSeq  uint64
	closedBy bool
}

func marshalProducer(s producerState) []byte {
	b := make([]byte, 17)
	binary.BigEndian.PutUint64(b[0:], s.epoch)
	binary.BigEndian.PutUint64(b[8:], s.lastSeq)
	if s.closedBy {
		b[16] = 1
	}
	return b
}

func unmarshalProducer(b []byte) (producerState, error) {
	if len(b) < 17 {
		return producerState{}, errBadMeta
	}
	return producerState{
		epoch:    binary.BigEndian.Uint64(b[0:]),
		lastSeq:  binary.BigEndian.Uint64(b[8:]),
		closedBy: b[16] != 0,
	}, nil
}

// --- RecordData value: flags:u8, data ---

const (
	recFlagNone byte = 0
)

func marshalRecord(flags byte, data []byte) []byte {
	b := make([]byte, 1+len(data))
	b[0] = flags
	copy(b[1:], data)
	return b
}

func recordPayload(v []byte) []byte {
	if len(v) == 0 {
		return nil
	}
	return v[1:]
}

// --- small integer value helpers ---

func appendU16(b []byte, v uint16) []byte {
	return append(b, byte(v>>8), byte(v))
}

func appendU64(b []byte, v uint64) []byte {
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], v)
	return append(b, tmp[:]...)
}

func u64Value(v uint64) []byte { return be64(v) }

func parseU64Value(b []byte) (uint64, error) {
	if len(b) < 8 {
		return 0, errBadMeta
	}
	return binary.BigEndian.Uint64(b), nil
}
