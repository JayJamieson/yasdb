package ds

import (
	"errors"
	"net/http"
	"time"
)

// segment is a contiguous run of sequence numbers [startSeq, endSeq) that
// lives in a single stream's own storage. Reading a fork stitches its
// inherited source segments and its own segment together.
type segment struct {
	id       uint64
	startSeq uint64
	endSeq   uint64
}

// resolveSegments maps the read range [startSeq, endSeq) of a stream to the
// concrete (streamID, seqRange) segments that hold the data. It walks the
// fork chain. The caller passes in the top-level fork info, so there is no
// DB read for the common non-fork case; deeper sources are loaded by id.
func (s *Server) resolveSegments(id, forkedFrom, forkOffset, startSeq, endSeq uint64) ([]segment, error) {
	if startSeq >= endSeq {
		return nil, nil
	}
	if forkedFrom == 0 {
		return []segment{{id: id, startSeq: startSeq, endSeq: endSeq}}, nil
	}
	var segs []segment
	if startSeq < forkOffset {
		inhEnd := endSeq
		if forkOffset < inhEnd {
			inhEnd = forkOffset
		}
		inh, err := s.resolveSegmentsByID(forkedFrom, startSeq, inhEnd)
		if err != nil {
			return nil, err
		}
		segs = append(segs, inh...)
	}
	if endSeq > forkOffset {
		ownStart := startSeq
		if ownStart < forkOffset {
			ownStart = forkOffset
		}
		segs = append(segs, segment{id: id, startSeq: ownStart, endSeq: endSeq})
	}
	return segs, nil
}

func (s *Server) resolveSegmentsByID(id, startSeq, endSeq uint64) ([]segment, error) {
	meta, _, ok, err := s.loadMetaByID(id)
	if err != nil {
		return nil, err
	}
	if !ok {
		// The source vanished. This should not happen: forks keep sources
		// alive. Treat the range as empty rather than erroring.
		return nil, nil
	}
	return s.resolveSegments(id, meta.ForkedFrom, meta.ForkOffset, startSeq, endSeq)
}

// loadMetaByID resolves a stream id to its path and metadata.
func (s *Server) loadMetaByID(id uint64) (*StreamMeta, string, bool, error) {
	pathB, found, err := s.store.Get(idMappingKey(id))
	if err != nil || !found {
		return nil, "", false, err
	}
	path := string(pathB)
	meta, ok, err := s.loadMeta(path)
	return meta, path, ok, err
}

// recordAt returns the record payload at (streamID, seq), following the fork
// chain so a materialised prefix can be read from wherever it actually lives.
func (s *Server) recordAt(id, seq uint64) ([]byte, bool, error) {
	segs, err := s.resolveSegmentsByID(id, seq, seq+1)
	if err != nil {
		return nil, false, err
	}
	if len(segs) == 0 {
		return nil, false, nil
	}
	seg := segs[len(segs)-1]
	v, found, err := s.store.Get(recordKey(seg.id, seq))
	if err != nil || !found {
		return nil, false, err
	}
	return recordPayload(v), true, nil
}

// --- reference counting (own key; never touched by the streamer) ---

func (s *Server) loadRefCount(id uint64) uint64 {
	if v, found, err := s.store.Get(refCountKey(id)); err == nil && found {
		if n, err := parseU64Value(v); err == nil {
			return n
		}
	}
	return 0
}

// --- fork creation ---

type forkParams struct {
	path          string
	sourcePath    string
	forkOffsetRaw string // "" = default to source tail
	subOffsetRaw  string // "" = absent
	subOffsetSet  bool
	contentType   string // "" = inherit source
	ttl           uint64
	ttlSet        bool
	expiresAt     int64
	expiresSet    bool
	closed        bool
	body          []byte
}

// createFork creates a forked stream. Holds metaMu.
func (s *Server) createFork(p forkParams) createResult {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()

	// Target path blocked by a soft-delete tombstone.
	if reason, _, found, err := s.loadTombstone(p.path); err != nil {
		return createResult{status: 500}
	} else if found && reason == tombSoftDelete {
		return createResult{status: http.StatusConflict}
	}

	// Resolve the source stream.
	if reason, _, found, err := s.loadTombstone(p.sourcePath); err != nil {
		return createResult{status: 500}
	} else if found && reason == tombSoftDelete {
		return createResult{status: http.StatusConflict} // fork from soft-deleted source
	}
	src, srcOK, err := s.loadMeta(p.sourcePath)
	if err != nil {
		return createResult{status: 500}
	}
	if !srcOK {
		return createResult{status: http.StatusNotFound}
	}
	sourceID := src.StreamID
	sourceTail, err := s.loadTailSeq(sourceID)
	if err != nil {
		return createResult{status: 500}
	}
	sourceJSON := isJSONStream(src.ContentType)

	// Content-type: inherit unless provided, in which case it must match.
	if p.contentType != "" && !contentTypeMatches(src.ContentType, p.contentType) {
		return createResult{status: http.StatusConflict}
	}
	contentType := src.ContentType
	if p.contentType != "" {
		contentType = p.contentType
	}

	// Fork offset (defaults to the source tail).
	forkOff := Offset{Seq: sourceTail}
	if p.forkOffsetRaw != "" {
		po, perr := parseOffset(p.forkOffsetRaw)
		if perr != nil {
			return createResult{status: http.StatusBadRequest, msg: "invalid fork offset"}
		}
		if po.isNow {
			forkOff = Offset{Seq: sourceTail}
		} else {
			forkOff = po.off
		}
	}
	if forkOff.Seq > sourceTail || (forkOff.Seq == sourceTail && forkOff.Byte > 0) {
		return createResult{status: http.StatusBadRequest, msg: "fork offset beyond stream length"}
	}

	// Sub-offset refinement.
	var subOffset uint64
	if p.subOffsetSet {
		n, valid := parseStrictUint(p.subOffsetRaw)
		if !valid {
			return createResult{status: http.StatusBadRequest, msg: "invalid sub-offset"}
		}
		if n > 0 && p.forkOffsetRaw == "" {
			return createResult{status: http.StatusBadRequest, msg: "sub-offset requires fork offset"}
		}
		if n > 0 && sourceTail == 0 {
			return createResult{status: http.StatusBadRequest, msg: "sub-offset on empty source"}
		}
		subOffset = n
	}

	// Resolve the effective divergence (divSeq, divByte). Materialisation
	// only happens for a mid-record binary divergence (divByte > 0).
	divSeq, divByte := forkOff.Seq, forkOff.Byte
	if sourceJSON {
		divSeq = forkOff.Seq + subOffset
		divByte = 0
		if divSeq > sourceTail {
			return createResult{status: http.StatusBadRequest, msg: "sub-offset overshoots message count"}
		}
	} else {
		combined := forkOff.Byte + subOffset
		if combined > 0 {
			if forkOff.Seq >= sourceTail {
				return createResult{status: http.StatusBadRequest, msg: "sub-offset overshoots message length"}
			}
			payload, found, rerr := s.recordAt(sourceID, forkOff.Seq)
			if rerr != nil {
				return createResult{status: 500}
			}
			if !found || combined > uint64(len(payload)) {
				return createResult{status: http.StatusBadRequest, msg: "sub-offset overshoots message length"}
			}
			if combined == uint64(len(payload)) {
				divSeq, divByte = forkOff.Seq+1, 0 // whole record inherited
			} else {
				divSeq, divByte = forkOff.Seq, combined
			}
		}
	}

	// Idempotent re-creation: an existing stream at the target must be the same
	// fork with matching config, else 409.
	if existing, ok, merr := s.loadMeta(p.path); merr != nil {
		return createResult{status: 500}
	} else if ok {
		reqTTL, reqExpires := forkTTL(src, p)
		matches := existing.ForkedFrom == sourceID && existing.ForkOffset == divSeq &&
			existing.ForkByte == divByte && contentTypeMatches(existing.ContentType, contentType) &&
			existing.TTLSeconds == reqTTL && existing.ExpiresAt == reqExpires && existing.Closed == p.closed
		if !matches {
			return createResult{status: http.StatusConflict}
		}
		tailSeq, terr := s.loadTailSeq(existing.StreamID)
		if terr != nil {
			return createResult{status: 500}
		}
		return createResult{status: http.StatusOK, contentType: existing.ContentType, nextOffset: tailOffset(tailSeq), closed: existing.Closed}
	}

	// New fork.
	id := s.nextID
	now := time.Now()
	ttlSecs, expiresAt := forkTTL(src, p)
	meta := &StreamMeta{
		StreamID:    id,
		ContentType: contentType,
		TTLSeconds:  ttlSecs,
		ExpiresAt:   expiresAt,
		Closed:      p.closed,
		CreatedAtMs: now.UnixMilli(),
		ForkedFrom:  sourceID,
		ForkOffset:  divSeq,
		ForkByte:    divByte,
	}
	if ttlSecs > 0 {
		meta.Deadline = uint64(now.Unix()) + ttlSecs + 1
	} else if expiresAt > 0 {
		meta.Deadline = uint64(expiresAt)
	}

	ops := make([]Op, 0, 8)
	tail := divSeq

	// Materialise the partial prefix record when the divergence is mid-record.
	if divByte > 0 {
		payload, found, rerr := s.recordAt(sourceID, divSeq)
		if rerr != nil || !found {
			return createResult{status: 500}
		}
		ops = append(ops, Op{Key: recordKey(id, divSeq), Val: marshalRecord(recFlagNone, payload[:divByte])})
		tail = divSeq + 1
	}

	// Initial body becomes the fork's own records after the divergence.
	if len(p.body) > 0 {
		records, berr := forkBodyRecords(contentType, p.body)
		if berr != nil {
			return createResult{status: http.StatusBadRequest, msg: "invalid body"}
		}
		for i, rec := range records {
			ops = append(ops, Op{Key: recordKey(id, tail+uint64(i)), Val: marshalRecord(recFlagNone, rec)})
		}
		tail += uint64(len(records))
	}

	ops = append(ops,
		Op{Key: metaKey(p.path), Val: meta.Marshal()},
		Op{Key: idMappingKey(id), Val: []byte(p.path)},
		Op{Key: tailKey(id), Val: marshalTail(tail, now.Unix())},
		Op{Key: idCounterKey(), Val: u64Value(id + 1)},
	)
	if meta.Deadline > 0 {
		ops = append(ops, Op{Key: expiryKey(meta.Deadline, id), Val: u64Value(ttlSecs)})
	}
	// Bump the source's reference count (its own key — no streamer race).
	ops = append(ops, Op{Key: refCountKey(sourceID), Val: u64Value(s.loadRefCount(sourceID) + 1)})

	if err := s.store.Commit(ops, true); err != nil {
		return createResult{status: 500}
	}
	s.nextID = id + 1

	return createResult{status: http.StatusCreated, contentType: contentType, nextOffset: tailOffset(tail), closed: p.closed}
}

// forkTTL applies the fork TTL/expiry inheritance table (protocol §4.2).
func forkTTL(src *StreamMeta, p forkParams) (ttlSecs uint64, expiresAt int64) {
	switch {
	case p.ttlSet:
		return p.ttl, 0
	case p.expiresSet:
		return 0, p.expiresAt
	case src.TTLSeconds > 0:
		return src.TTLSeconds, 0
	case src.ExpiresAt > 0:
		return 0, src.ExpiresAt
	default:
		return 0, 0
	}
}

// forkBodyRecords splits a fork's initial PUT body into records.
func forkBodyRecords(contentType string, body []byte) ([][]byte, error) {
	if isJSONStream(contentType) {
		recs, err := flattenJSON(body)
		if errors.Is(err, errEmptyArray) {
			return nil, nil
		}
		return recs, err
	}
	return [][]byte{body}, nil
}
