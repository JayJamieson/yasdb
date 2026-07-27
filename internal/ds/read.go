package ds

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// readChunkBytes is the default soft cap on bytes returned per read
// (Config.MaxReadBytes). A non-JSON record larger than the cap is delivered
// across reads via the offset byte-component; JSON messages are atomic (SPEC §4).
const readChunkBytes = 1 << 20

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, path string) {
	q := r.URL.Query()
	// An explicitly present but empty offset (?offset=) or a repeated offset
	// parameter is malformed (protocol §8).
	if vals, present := q["offset"]; present {
		if len(vals) > 1 || vals[0] == "" {
			http.Error(w, "malformed offset", http.StatusBadRequest)
			return
		}
	}
	rawOffset := q.Get("offset")
	live := q.Get("live")
	clientCursor := q.Get("cursor")

	po, err := parseOffset(rawOffset)
	if err != nil {
		http.Error(w, "malformed offset", http.StatusBadRequest)
		return
	}

	if reason, _, found, terr := s.loadTombstone(path); terr != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	} else if found && reason == tombSoftDelete {
		http.Error(w, "stream gone", http.StatusGone)
		return
	}

	meta, ok, err := s.loadMeta(path)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "stream not found", http.StatusNotFound)
		return
	}

	// Reads slide the TTL window (protocol §5.1). Best-effort via the streamer,
	// which owns meta; skipped entirely for non-TTL streams.
	if meta.TTLSeconds > 0 {
		s.touchStream(path)
	}

	switch live {
	case "":
		s.readCatchup(w, r, meta, po, rawOffset)
	case "long-poll":
		s.readLongPoll(w, r, path, meta, po, rawOffset, clientCursor)
	case "sse":
		s.readSSE(w, r, path, meta, po, rawOffset, clientCursor)
	default:
		http.Error(w, "invalid live mode", http.StatusBadRequest)
	}
}

// readRange assembles a response body for [start, tailSeq), accumulating up to
// ~limit bytes. For application/json, messages are atomic (byte offset always 0).
// For other content types, a record larger than the limit is delivered across
// reads via the offset byte component (SPEC §4). Reads stitch a fork's inherited
// source segments with its own data (SPEC §11).
func (s *Server) readRange(streamID, forkedFrom, forkOffset uint64, isJSON bool, start Offset, tailSeq, limit uint64) (body []byte, next Offset, upToDate bool, err error) {
	if start.Seq >= tailSeq {
		if isJSON {
			return []byte("[]"), start, true, nil
		}
		return nil, start, true, nil
	}
	segs, err := s.resolveSegments(streamID, forkedFrom, forkOffset, start.Seq, tailSeq)
	if err != nil {
		return nil, start, false, err
	}
	if isJSON {
		return s.readSegmentsJSON(segs, start, tailSeq, limit)
	}
	return s.readSegmentsBinary(segs, start, tailSeq, limit)
}

// readSegmentsBinary assembles raw bytes across (streamID, seqRange) segments
// with byte-accurate offsets, splitting a record across reads at the limit.
func (s *Server) readSegmentsBinary(segs []segment, start Offset, tailSeq, limit uint64) ([]byte, Offset, bool, error) {
	var buf bytes.Buffer
	next := start
	for _, seg := range segs {
		it, err := s.store.Scan(recordKey(seg.id, seg.startSeq), recordPrefixEnd(seg.id))
		if err != nil {
			return nil, start, false, err
		}
		stop := false
		for uint64(buf.Len()) < limit {
			k, v, ok, err := it.Next()
			if err != nil {
				it.Close()
				return nil, start, false, err
			}
			if !ok {
				break
			}
			recSeq := be64Decode(k[9:17])
			if recSeq >= seg.endSeq {
				break
			}
			payload := recordPayload(v)
			var skip uint64
			if recSeq == start.Seq {
				skip = start.Byte
			}
			if skip > uint64(len(payload)) {
				skip = uint64(len(payload))
			}
			avail := payload[skip:]
			room := limit - uint64(buf.Len())
			if uint64(len(avail)) <= room {
				buf.Write(avail)
				next = Offset{Seq: recSeq + 1}
			} else {
				buf.Write(avail[:room])
				next = Offset{Seq: recSeq, Byte: skip + room}
				stop = true
				break
			}
		}
		it.Close()
		if stop || uint64(buf.Len()) >= limit {
			break
		}
	}
	return buf.Bytes(), next, next.Seq >= tailSeq, nil
}

// readSegmentsJSON collects whole JSON messages across segments and re-wraps them
// in a single array (SPEC §9.1, §11).
func (s *Server) readSegmentsJSON(segs []segment, start Offset, tailSeq, limit uint64) ([]byte, Offset, bool, error) {
	var recs [][]byte
	var total uint64
	nextSeq := start.Seq
	stop := false
	for _, seg := range segs {
		if stop {
			break
		}
		it, err := s.store.Scan(recordKey(seg.id, seg.startSeq), recordPrefixEnd(seg.id))
		if err != nil {
			return nil, start, false, err
		}
		for {
			k, v, ok, err := it.Next()
			if err != nil {
				it.Close()
				return nil, start, false, err
			}
			if !ok {
				break
			}
			recSeq := be64Decode(k[9:17])
			if recSeq >= seg.endSeq {
				break
			}
			payload := recordPayload(v)
			if len(recs) > 0 && total+uint64(len(payload)) > limit {
				stop = true
				break
			}
			recs = append(recs, payload)
			nextSeq = recSeq + 1
			total += uint64(len(payload))
			if total >= limit {
				stop = true
				break
			}
		}
		it.Close()
	}
	return wrapJSONArray(recs), Offset{Seq: nextSeq}, nextSeq >= tailSeq, nil
}

// --- catch-up ---

func (s *Server) readCatchup(w http.ResponseWriter, r *http.Request, meta *StreamMeta, po parsedOffset, rawOffset string) {
	isJSON := isJSONStream(meta.ContentType)
	tail, closed, _, _, err := s.liveState(rPath(r))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set(hContentType, meta.ContentType)

	// offset=now: skip to tail, never cache, no ETag.
	if po.isNow {
		h.Set(hCacheControl, "no-store")
		h.Set(hStreamNext, tailOffset(tail).String())
		h.Set(hStreamUpToDate, "true")
		if closed {
			h.Set(hStreamClosed, "true")
		}
		w.WriteHeader(http.StatusOK)
		if isJSON {
			w.Write([]byte("[]"))
		}
		return
	}

	body, next, upToDate, err := s.readRange(meta.StreamID, meta.ForkedFrom, meta.ForkOffset, isJSON, po.off, tail, s.cfg.MaxReadBytes)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	closedEOF := closed && upToDate

	etag := makeETag(meta.StreamID, po.off, next, closedEOF)
	h.Set(hETag, etag)
	h.Set(hCacheControl, cacheControl(meta.IsPrivate))
	h.Set(hStreamNext, next.String())
	if upToDate {
		h.Set(hStreamUpToDate, "true")
	}
	if closedEOF {
		h.Set(hStreamClosed, "true")
	}

	if match := r.Header.Get(hIfNoneMatch); match != "" && etagMatches(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

// --- long-poll ---

func (s *Server) readLongPoll(w http.ResponseWriter, r *http.Request, path string, meta *StreamMeta, po parsedOffset, rawOffset, clientCursor string) {
	if rawOffset == "" {
		http.Error(w, "offset is required for live reads", http.StatusBadRequest)
		return
	}
	st, found, err := s.getOrSpawn(path)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "stream not found", http.StatusNotFound)
		return
	}
	isJSON := st.isJSON
	st.readers.Add(1)
	defer st.readers.Add(-1)

	start := po.off
	if po.isNow {
		start = Offset{Seq: st.tail.Load()}
	}

	timer := time.NewTimer(s.cfg.LongPollTimeout)
	defer timer.Stop()

	for {
		wc := st.waiterChan()
		tail := st.tail.Load()
		closed := st.closed.Load()

		body, next, upToDate, ok := st.cacheRead(nil, isJSON, start, tail, s.cfg.MaxReadBytes)
		if !ok {
			var err error
			body, next, upToDate, err = s.readRange(st.id, st.forkedFrom, st.forkOffset, isJSON, start, tail, s.cfg.MaxReadBytes)
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
		if st.stopped() {
			http.Error(w, "stream not found", http.StatusNotFound)
			return
		}
		if next != start { // data delivered
			s.writeLiveData(w, meta, start, next, upToDate, closed, body, clientCursor)
			return
		}
		if closed {
			s.write204Closed(w, tail)
			return
		}
		select {
		case <-wc:
			continue
		case <-st.stop:
			// Stream was deleted (or server is shutting down): stop serving it.
			http.Error(w, "stream not found", http.StatusNotFound)
			return
		case <-timer.C:
			s.write204Timeout(w, st.tail.Load(), clientCursor)
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) writeLiveData(w http.ResponseWriter, meta *StreamMeta, start, next Offset, upToDate, closed bool, body []byte, clientCursor string) {
	closedEOF := closed && upToDate
	h := w.Header()
	h.Set(hContentType, meta.ContentType)
	h.Set(hETag, makeETag(meta.StreamID, start, next, closedEOF))
	h.Set(hCacheControl, cacheControl(meta.IsPrivate))
	h.Set(hStreamNext, next.String())
	h.Set(hStreamCursor, computeCursor(clientCursor, time.Now()))
	if upToDate {
		h.Set(hStreamUpToDate, "true")
	}
	if closedEOF {
		h.Set(hStreamClosed, "true")
	}
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func (s *Server) write204Closed(w http.ResponseWriter, tail uint64) {
	h := w.Header()
	h.Set(hStreamNext, tailOffset(tail).String())
	h.Set(hStreamUpToDate, "true")
	h.Set(hStreamClosed, "true")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) write204Timeout(w http.ResponseWriter, tail uint64, clientCursor string) {
	h := w.Header()
	h.Set(hStreamNext, tailOffset(tail).String())
	h.Set(hStreamUpToDate, "true")
	h.Set(hStreamCursor, computeCursor(clientCursor, time.Now()))
	w.WriteHeader(http.StatusNoContent)
}

// --- SSE ---

// sseControl is the per-delivery SSE control event. Its JSON is hand-rolled
// (appendSSEControl) rather than json.Marshal'd: the fields are an offset
// (digits + '_') and a cursor (decimal digits), both escape-free, so the output
// is byte-identical to json.Marshal of the equivalent tagged struct — guarded by
// TestSSEControlFrameMatchesJSON.
type sseControl struct {
	Next         Offset // streamNextOffset
	StreamCursor string // streamCursor, omitempty
	UpToDate     bool   // upToDate, omitempty
	StreamClosed bool   // streamClosed, omitempty
}

func (s *Server) readSSE(w http.ResponseWriter, r *http.Request, path string, meta *StreamMeta, po parsedOffset, rawOffset, clientCursor string) {
	if rawOffset == "" {
		http.Error(w, "offset is required for live reads", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	st, found, err := s.getOrSpawn(path)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "stream not found", http.StatusNotFound)
		return
	}
	isJSON := st.isJSON
	st.readers.Add(1)
	defer st.readers.Add(-1)
	textLike := isTextLike(meta.ContentType)

	h := w.Header()
	h.Set(hContentType, "text/event-stream")
	h.Set(hCacheControl, "no-cache")
	h.Set("Connection", "keep-alive")
	if !textLike {
		h.Set(hSSEDataEncoding, "base64")
	}
	w.WriteHeader(http.StatusOK)

	start := po.off
	if po.isNow {
		start = Offset{Seq: st.tail.Load()}
	}

	timer := time.NewTimer(s.cfg.SSELifetime)
	defer timer.Stop()
	first := true
	// Scratch buffers reused across loop iterations so a caught-up reader's steady
	// state allocates neither a body nor a control frame per delivery.
	var dataBuf, ctrlBuf []byte

	for {
		wc := st.waiterChan()
		tail := st.tail.Load()
		closed := st.closed.Load()

		body, next, upToDate, ok := st.cacheRead(dataBuf[:0], isJSON, start, tail, s.cfg.MaxReadBytes)
		if ok {
			dataBuf = body // keep the (possibly grown) backing array for reuse
		} else {
			var err error
			body, next, upToDate, err = s.readRange(st.id, st.forkedFrom, st.forkOffset, isJSON, start, tail, s.cfg.MaxReadBytes)
			if err != nil {
				return
			}
		}
		if st.stopped() {
			return
		}
		if next != start { // data delivered
			closedEOF := closed && upToDate
			writeSSEData(w, textLike, body)
			ctrlBuf = writeSSEControl(w, ctrlBuf, sseControl{
				Next:         next,
				StreamCursor: cursorUnlessClosed(clientCursor, closedEOF),
				UpToDate:     upToDate,
				StreamClosed: closedEOF,
			})
			flusher.Flush()
			start = next
			first = false
			if closedEOF {
				return
			}
			continue
		}
		if closed {
			ctrlBuf = writeSSEControl(w, ctrlBuf, sseControl{
				Next:         tailOffset(tail),
				UpToDate:     true,
				StreamClosed: true,
			})
			flusher.Flush()
			return
		}
		if first {
			ctrlBuf = writeSSEControl(w, ctrlBuf, sseControl{
				Next:         tailOffset(tail),
				StreamCursor: computeCursor(clientCursor, time.Now()),
				UpToDate:     true,
			})
			flusher.Flush()
			first = false
		}
		select {
		case <-wc:
			continue
		case <-st.stop:
			// Stream deleted / server shutting down: end the SSE stream.
			return
		case <-timer.C:
			return
		case <-r.Context().Done():
			return
		}
	}
}

// Preallocated SSE framing literals (hoisted so each write doesn't allocate a
// fresh []byte from a string constant).
var (
	sseEventData       = []byte("event: data\n")
	sseDataPrefix      = []byte("data:")
	sseNL              = []byte("\n")
	sseNLNL            = []byte("\n\n")
	sseControlHead     = []byte("event: control\ndata:{\"streamNextOffset\":\"")
	sseControlCursor   = []byte(",\"streamCursor\":\"")
	sseControlUpToDate = []byte(",\"upToDate\":true")
	sseControlClosed   = []byte(",\"streamClosed\":true")
)

func writeSSEData(w http.ResponseWriter, textLike bool, payload []byte) {
	w.Write(sseEventData)
	if !textLike {
		// base64 has no CR/LF, so it is always a single safe data field.
		w.Write(sseDataPrefix)
		enc := base64.StdEncoding
		buf := make([]byte, enc.EncodedLen(len(payload)))
		enc.Encode(buf, payload)
		w.Write(buf)
		w.Write(sseNLNL)
		return
	}
	// Fast path (the common case: a batch of newline-free records): the payload is
	// one verbatim data line, written directly with no string conversion.
	if bytes.IndexByte(payload, '\n') < 0 && bytes.IndexByte(payload, '\r') < 0 {
		w.Write(sseDataPrefix)
		w.Write(payload)
		w.Write(sseNLNL)
		return
	}
	// Slow path: every CR, LF, and CRLF is an SSE line boundary, so we split on all
	// of them and emit one `data:` field per line. This preserves the payload's own
	// newlines as data and makes it impossible for an embedded blank line
	// ("\r\n\r\n") to be read as an event boundary (CRLF/LF injection). No space
	// after `data:` — the value is verbatim.
	normalized := strings.ReplaceAll(string(payload), "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	for _, line := range strings.Split(normalized, "\n") {
		w.Write(sseDataPrefix)
		w.Write([]byte(line))
		w.Write(sseNL)
	}
	w.Write(sseNL)
}

// writeSSEControl writes the control event, assembling its frame into dst (the
// caller's reusable scratch) and returning the grown buffer for reuse.
func writeSSEControl(w http.ResponseWriter, dst []byte, c sseControl) []byte {
	dst = appendSSEControl(dst[:0], c)
	w.Write(dst)
	return dst
}

// appendSSEControl appends the full `event: control\ndata:{…}\n\n` frame to dst.
// Hand-rolled instead of json.Marshal; byte-identical because every value is
// escape-free (offset digits/'_' and a decimal cursor).
func appendSSEControl(dst []byte, c sseControl) []byte {
	dst = append(dst, sseControlHead...)
	dst = appendOffset(dst, c.Next)
	dst = append(dst, '"')
	if c.StreamCursor != "" {
		dst = append(dst, sseControlCursor...)
		dst = append(dst, c.StreamCursor...)
		dst = append(dst, '"')
	}
	if c.UpToDate {
		dst = append(dst, sseControlUpToDate...)
	}
	if c.StreamClosed {
		dst = append(dst, sseControlClosed...)
	}
	return append(dst, '}', '\n', '\n')
}

func cursorUnlessClosed(clientCursor string, closed bool) string {
	if closed {
		return ""
	}
	return computeCursor(clientCursor, time.Now())
}

// --- shared helpers ---

func cacheControl(private bool) string {
	if private {
		return "private, max-age=60, stale-while-revalidate=300"
	}
	return "public, max-age=60, stale-while-revalidate=300"
}

// makeETag builds {streamID}:{startOffset}:{endOffset}[:c] (protocol §10.1). The
// `:c` suffix is mandatory when closed so a 304 can never hide EOF.
func makeETag(id uint64, start, end Offset, closed bool) string {
	tag := fmt.Sprintf(`"%d:%s:%s`, id, start.String(), end.String())
	if closed {
		tag += ":c"
	}
	return tag + `"`
}

func etagMatches(ifNoneMatch, etag string) bool {
	for _, part := range strings.Split(ifNoneMatch, ",") {
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, "W/")
		if part == etag || part == "*" {
			return true
		}
	}
	return false
}

func rPath(r *http.Request) string { return r.URL.Path }
