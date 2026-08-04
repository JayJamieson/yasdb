package ds

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/btree"
)

// pwalStore is an experimental alternative Storage backend, built to test
// one specific idea. Instead of an LSM engine (SlateDB) with a memtable and
// compaction, it stages every append, across every stream, into one of N
// independent WAL shards. A shard fsyncs immediately when new data arrives.
// While that fsync is in flight, any further writes to the same shard just
// buffer and ride the *next* commit, issued the instant the previous one
// finishes. This is adaptive group commit scaled to contention, not a fixed
// timer.
//
// There is no SST tier. Unlike an initial version of this file, though,
// values are NOT kept in RAM: the in-memory btree indexes key -> (shard,
// file offset, length) only, and Get/Scan read the payload bytes back from
// the shard file on demand. "Store bytes on disk in the format they are
// sent over the wire — a read is a byte-range over a file" is the design
// principle from the source this is modelled on. Keeping full values
// in-process defeated that. Memory now scales with record *count*, not
// total payload bytes.
//
// This mirrors the design described for the Rust durable-streams reference
// server ("Durable Streams at Kernel Speed",
// https://electric.ax/blog/2026/06/26/durable-streams-at-kernel-speed):
// "Every append, across every stream, is staged into a sharded WAL. We call
// fsync immediately when new data arrives for a WAL shard. While a flush is
// in progress, we batch other incoming requests and commit them together
// once the previous batch finishes." Sharding is global (round-robin
// across every commit, not per-stream), specifically so a single hot
// stream's pipelined commits can spread across shards instead of
// serialising through one WAL. See BENCHMARKS.md for why that mattered
// here.
//
// It satisfies the same Storage interface as slateStore, so it is a
// drop-in swap for the record/append path in a load test. It is not a
// general storage engine: it has no compaction, no object storage, and
// still an unbounded index (one small entry per key, forever, no
// eviction). Use SlateDB for other stateful needs, per the same reasoning
// that applied to the earlier objwal PoC.
type pwalStore struct {
	shards    []*pwalShard
	nextShard atomic.Uint64 // round-robin shard selector, independent of seq

	mu   sync.RWMutex
	tree *btree.BTreeG[pwalEntry]

	errMu sync.Mutex
	err   error

	wm *contigWatermark
}

// pwalEntry is the in-memory index record: everything needed to read a key's
// current value back off disk, but not the value itself.
type pwalEntry struct {
	key      []byte
	shardIdx int
	fileOff  int64
	n        int32
}

func pwalLess(a, b pwalEntry) bool { return bytes.Compare(a.key, b.key) < 0 }

// pwalShard is one independent WAL file with its own group-commit
// pipeline. Global commit sequence numbers are constructed as
// local*numShards+shardIdx+1, so a shard's own sequence assignment is
// always monotonic, while staying unique across shards.
type pwalShard struct {
	idx, numShards int
	f              *os.File
	onErr          func(error)
	onDurable      func(seqs []uint64)

	mu          sync.Mutex
	localSeq    uint64
	writeOffset int64
	pending     []uint64 // seqs written but not yet fsynced, ascending
	inFlight    bool
}

// stage encodes ops as one WAL record under this shard's own sequence
// slot, and writes it: a plain write(2), visible to a subsequent read on
// this fd immediately, well before fsync. So the returned locations are
// valid the instant this call returns. It also ensures a durability flush
// is running for the shard: starting one now if it was idle, otherwise
// this record's seq rides the flush already in flight.
func (sh *pwalShard) stage(ops []Op) (seq uint64, locs []pwalValueLoc) {
	sh.mu.Lock()
	local := sh.localSeq
	sh.localSeq++
	seq = local*uint64(sh.numShards) + uint64(sh.idx) + 1 // 0 stays "nothing durable yet"

	rec, relOff, relLen := encodePWALRecord(seq, ops)
	base := sh.writeOffset
	if _, err := sh.f.Write(rec); err != nil {
		sh.onErr(fmt.Errorf("pwal shard %d: write: %w", sh.idx, err))
	}
	sh.writeOffset += int64(len(rec))

	locs = make([]pwalValueLoc, 0, len(ops))
	for i, op := range ops {
		if op.Del {
			continue
		}
		locs = append(locs, pwalValueLoc{key: op.Key, fileOff: base + int64(relOff[i]), n: relLen[i]})
	}

	sh.pending = append(sh.pending, seq)
	if sh.inFlight {
		sh.mu.Unlock()
		return seq, locs
	}
	sh.inFlight = true
	toFlushSeqs := sh.pending
	sh.pending = nil
	sh.mu.Unlock()
	go sh.runFlush(toFlushSeqs)
	return seq, locs
}

// runFlush fsyncs, publishes durability for the batch of seqs written
// before this call started, then loops onto whatever was staged while it
// ran. This is the "batch other incoming requests and commit them together
// once the previous batch finishes" discipline. It exits once the shard is
// caught up.
func (sh *pwalShard) runFlush(seqs []uint64) {
	for {
		if err := sh.f.Sync(); err != nil {
			sh.onErr(fmt.Errorf("pwal shard %d: fsync: %w", sh.idx, err))
		} else if len(seqs) > 0 {
			sh.onDurable(seqs)
		}
		sh.mu.Lock()
		if len(sh.pending) == 0 {
			sh.inFlight = false
			sh.mu.Unlock()
			return
		}
		seqs = sh.pending
		sh.pending = nil
		sh.mu.Unlock()
	}
}

// pwalValueLoc is a value's absolute byte range within its shard file,
// computed once at write time and handed back so the caller can index it.
type pwalValueLoc struct {
	key     []byte
	fileOff int64
	n       int32
}

// --- WAL record encoding: [len u32][seq u64][crc32c u32][opCount u32]{op}* ---
// each op: [del u8][keyLen u32][key][valLen u32][val] (valLen/val omitted when del)

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// encodePWALRecord returns the full record bytes, plus, per op (same order
// and length as ops), the byte offset and length of that op's value
// *within rec*. Both are 0 for deletes, which have no value.
func encodePWALRecord(seq uint64, ops []Op) (rec []byte, relOff []int32, relLen []int32) {
	const headerLen = 4 + 8 + 4 // length prefix + seq + crc

	var payload []byte
	payload = binary.BigEndian.AppendUint32(payload, uint32(len(ops)))
	relOff = make([]int32, len(ops))
	relLen = make([]int32, len(ops))
	for i, op := range ops {
		if op.Del {
			payload = append(payload, 1)
		} else {
			payload = append(payload, 0)
		}
		payload = binary.BigEndian.AppendUint32(payload, uint32(len(op.Key)))
		payload = append(payload, op.Key...)
		if !op.Del {
			payload = binary.BigEndian.AppendUint32(payload, uint32(len(op.Val)))
			relOff[i] = int32(headerLen + len(payload))
			relLen[i] = int32(len(op.Val))
			payload = append(payload, op.Val...)
		}
	}
	crc := crc32.Checksum(payload, crc32cTable)

	rec = make([]byte, 0, headerLen+len(payload))
	rec = binary.BigEndian.AppendUint32(rec, uint32(8+4+len(payload)))
	rec = binary.BigEndian.AppendUint64(rec, seq)
	rec = binary.BigEndian.AppendUint32(rec, crc)
	rec = append(rec, payload...)
	return rec, relOff, relLen
}

// pwalRecEntry is one decoded op from replay. It carries enough to rebuild
// the in-memory index (key -> shard/offset/length), and, for a key seen
// more than once across the file, to let the last write win. This is
// exactly like applying Op directly, just without holding the value bytes.
type pwalRecEntry struct {
	key     []byte
	del     bool
	fileOff int64
	n       int32
}

type pwalRecord struct {
	seq     uint64
	entries []pwalRecEntry
}

// decodePWALShard replays one shard file into its committed records
// without copying value bytes: each entry carries the file offset and
// length instead, mirroring exactly what stage() computes at write time
// (see the comment there for the byte-accounting this must match). A
// truncated or corrupt (bad checksum) trailing record stops replay at that
// point, rather than erroring. This is standard WAL recovery semantics: it
// is an in-flight write that never finished.
func decodePWALShard(data []byte) []pwalRecord {
	var recs []pwalRecord
	filePos := int64(0)
	for filePos+4 <= int64(len(data)) {
		n := binary.BigEndian.Uint32(data[filePos : filePos+4])
		recBodyStart := filePos + 4
		if recBodyStart+int64(n) > int64(len(data)) {
			break
		}
		buf := data[recBodyStart : recBodyStart+int64(n)]
		if len(buf) < 12 {
			break
		}
		seq := binary.BigEndian.Uint64(buf[0:8])
		crc := binary.BigEndian.Uint32(buf[8:12])
		payload := buf[12:]
		if crc32.Checksum(payload, crc32cTable) != crc {
			break
		}
		if len(payload) < 4 {
			break
		}
		opCount := binary.BigEndian.Uint32(payload[0:4])
		payloadFileStart := recBodyStart + 12
		cur := 4 // index into payload, past the opCount field
		entries := make([]pwalRecEntry, 0, opCount)
		ok := true
	opLoop:
		for i := uint32(0); i < opCount; i++ {
			if cur+1 > len(payload) {
				ok = false
				break opLoop
			}
			del := payload[cur] == 1
			cur++
			if cur+4 > len(payload) {
				ok = false
				break opLoop
			}
			keyLen := int(binary.BigEndian.Uint32(payload[cur : cur+4]))
			cur += 4
			if cur+keyLen > len(payload) {
				ok = false
				break opLoop
			}
			key := append([]byte(nil), payload[cur:cur+keyLen]...)
			cur += keyLen
			var fileOff int64
			var vn int32
			if !del {
				if cur+4 > len(payload) {
					ok = false
					break opLoop
				}
				valLen := int(binary.BigEndian.Uint32(payload[cur : cur+4]))
				cur += 4
				if cur+valLen > len(payload) {
					ok = false
					break opLoop
				}
				fileOff = payloadFileStart + int64(cur)
				vn = int32(valLen)
				cur += valLen
			}
			entries = append(entries, pwalRecEntry{key: key, del: del, fileOff: fileOff, n: vn})
		}
		if !ok {
			break
		}
		recs = append(recs, pwalRecord{seq: seq, entries: entries})
		filePos = recBodyStart + int64(n)
	}
	return recs
}

// --- pwalStore ---

// OpenPWALStore opens (or creates) a sharded-WAL store at dir with the given
// number of shards. See pwalStore's doc comment for the design.
func OpenPWALStore(dir string, shardCount int) (Storage, error) {
	if shardCount < 1 {
		shardCount = 1
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("pwal: mkdir: %w", err)
	}

	s := &pwalStore{tree: btree.NewG(32, pwalLess), wm: newContigWatermark()}

	allByShard := make([][]pwalRecord, shardCount)
	for i := 0; i < shardCount; i++ {
		path := filepath.Join(dir, fmt.Sprintf("shard-%03d.wal", i))
		if existing, err := os.ReadFile(path); err == nil && len(existing) > 0 {
			allByShard[i] = decodePWALShard(existing)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
		if err != nil {
			return nil, fmt.Errorf("pwal: open shard %d: %w", i, err)
		}
		info, err := f.Stat()
		if err != nil {
			return nil, fmt.Errorf("pwal: stat shard %d: %w", i, err)
		}
		s.shards = append(s.shards, &pwalShard{
			idx: i, numShards: shardCount, f: f, writeOffset: info.Size(),
			onErr: s.setErr, onDurable: s.wm.mark,
		})
	}

	var all []pwalRecord
	var recoveredSeqs []uint64
	for i, recs := range allByShard {
		all = append(all, recs...)
		if len(recs) > 0 {
			last := recs[len(recs)-1].seq
			s.shards[i].localSeq = (last-uint64(i)-1)/uint64(shardCount) + 1
			for _, r := range recs {
				recoveredSeqs = append(recoveredSeqs, r.seq)
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].seq < all[j].seq })
	for _, r := range all {
		// Which shard this record came from follows the same seq bijection
		// stage() uses ((seq-1)%numShards == shardIdx). So it does not need
		// to be threaded through the sort/merge above.
		shardIdx := int((r.seq - 1) % uint64(shardCount))
		for _, e := range r.entries {
			if e.del {
				s.tree.Delete(pwalEntry{key: e.key})
			} else {
				s.tree.ReplaceOrInsert(pwalEntry{key: e.key, shardIdx: shardIdx, fileOff: e.fileOff, n: e.n})
			}
		}
	}
	// Everything replayed from a shard file was fsynced there (a truncated
	// tail record is dropped by decodePWALShard), so it is all durable.
	// Seed the watermark the same contiguous-prefix way live commits do.
	s.wm.mark(recoveredSeqs)

	return s, nil
}

func (s *pwalStore) setErr(err error) {
	s.errMu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.errMu.Unlock()
}

func (s *pwalStore) firstErr() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *pwalStore) readValue(e pwalEntry) ([]byte, error) {
	buf := make([]byte, e.n)
	if _, err := s.shards[e.shardIdx].f.ReadAt(buf, e.fileOff); err != nil {
		return nil, fmt.Errorf("pwal: read value: %w", err)
	}
	return buf, nil
}

func (s *pwalStore) Get(key []byte) ([]byte, bool, error) {
	s.mu.RLock()
	e, ok := s.tree.Get(pwalEntry{key: key})
	s.mu.RUnlock()
	if !ok {
		return nil, false, nil
	}
	v, err := s.readValue(e)
	if err != nil {
		return nil, false, err
	}
	return v, true, nil
}

func (s *pwalStore) CommitAsync(ops []Op) (uint64, error) {
	if err := s.firstErr(); err != nil {
		return 0, err
	}
	shardIdx := int(s.nextShard.Add(1)-1) % len(s.shards)
	seq, locs := s.shards[shardIdx].stage(ops)

	byKey := make(map[string]pwalValueLoc, len(locs))
	for _, l := range locs {
		byKey[string(l.key)] = l
	}
	s.mu.Lock()
	for _, op := range ops {
		if op.Del {
			s.tree.Delete(pwalEntry{key: op.Key})
			continue
		}
		l := byKey[string(op.Key)]
		s.tree.ReplaceOrInsert(pwalEntry{
			key:      append([]byte(nil), op.Key...),
			shardIdx: shardIdx,
			fileOff:  l.fileOff,
			n:        l.n,
		})
	}
	s.mu.Unlock()

	return seq, nil
}

func (s *pwalStore) Commit(ops []Op, awaitDurable bool) error {
	seq, err := s.CommitAsync(ops)
	if err != nil {
		return err
	}
	if !awaitDurable {
		return nil
	}
	for {
		d, err := s.DurableSeq()
		if err != nil {
			return err
		}
		if d >= seq {
			return nil
		}
		time.Sleep(200 * time.Microsecond)
	}
}

// DurableSeq is the largest N such that every commit up to and including N
// is durable. This is the contiguous prefix wm maintains as shards' flushes
// complete out of order relative to each other.
func (s *pwalStore) DurableSeq() (uint64, error) {
	if err := s.firstErr(); err != nil {
		return 0, err
	}
	return s.wm.get(), nil
}

type pwalIterator struct {
	store   *pwalStore
	entries []pwalEntry
	idx     int
}

func (it *pwalIterator) Next() (key, val []byte, ok bool, err error) {
	if it.idx >= len(it.entries) {
		return nil, nil, false, nil
	}
	e := it.entries[it.idx]
	it.idx++
	v, err := it.store.readValue(e)
	if err != nil {
		return nil, nil, false, err
	}
	return e.key, v, true, nil
}

func (it *pwalIterator) Close() {}

func (s *pwalStore) Scan(start, end []byte) (Iterator, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var entries []pwalEntry
	visit := func(e pwalEntry) bool {
		entries = append(entries, e)
		return true
	}
	if end == nil {
		s.tree.AscendGreaterOrEqual(pwalEntry{key: start}, visit)
	} else {
		s.tree.AscendRange(pwalEntry{key: start}, pwalEntry{key: end}, visit)
	}
	return &pwalIterator{store: s, entries: entries}, nil
}

func (s *pwalStore) Close() error {
	var firstErr error
	for _, sh := range s.shards {
		if err := sh.f.Sync(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := sh.f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
