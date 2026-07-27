package ds

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ServeHTTP routes a request to the matching stream operation. The request path
// is the stream identifier; any URL scheme works (protocol §3). The reserved
// `__ds` prefix (subscriptions) is not implemented.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if r.Method == http.MethodOptions {
		// CORS preflight: allow any origin/method and all request headers so
		// browser clients can send If-None-Match, Producer-*, Stream-*, etc.
		h := w.Header()
		h.Set("Access-Control-Allow-Methods", "GET, HEAD, PUT, POST, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "*")
		h.Set("Access-Control-Max-Age", "86400")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	path := r.URL.Path
	if isReservedPath(path) {
		http.Error(w, "subscription APIs are not implemented", http.StatusNotImplemented)
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.handlePut(w, r, path)
	case http.MethodPost:
		s.handlePost(w, r, path)
	case http.MethodGet:
		s.handleGet(w, r, path)
	case http.MethodHead:
		s.handleHead(w, r, path)
	case http.MethodDelete:
		s.handleDelete(w, r, path)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT, POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	// Let browser clients from any origin read the protocol's custom headers.
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Expose-Headers", "*")
}

// absoluteURL builds an absolute URL for path from the request, for Location.
func absoluteURL(r *http.Request, path string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	return scheme + "://" + r.Host + path
}

// isReservedPath reports whether the first path segment is the reserved `__ds`.
func isReservedPath(path string) bool {
	seg := strings.TrimPrefix(path, "/")
	if i := strings.IndexByte(seg, '/'); i >= 0 {
		seg = seg[:i]
	}
	return seg == "__ds"
}

// --- PUT: create stream (or fork) ---

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request, path string) {
	ct := r.Header.Get(hContentType)
	ttl, ttlSet, expires, expiresSet, ok := parseExpiry(r)
	if !ok {
		http.Error(w, "invalid TTL/expiry", http.StatusBadRequest)
		return
	}
	closed := headerTrue(r.Header.Get(hStreamClosed))
	forkedFrom := r.Header.Get(hForkedFrom)
	subOffRaw := r.Header.Get(hForkSubOffset)

	// A sub-offset is only meaningful with a fork source (protocol §4.2).
	if forkedFrom == "" && subOffRaw != "" {
		http.Error(w, "Stream-Fork-Sub-Offset requires Stream-Forked-From", http.StatusBadRequest)
		return
	}

	body, tooLarge, err := readBody(w, r, s.cfg.MaxRecordBytes)
	if err != nil {
		if tooLarge {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var res createResult
	if forkedFrom != "" {
		res = s.createFork(forkParams{
			path: path, sourcePath: forkedFrom,
			forkOffsetRaw: r.Header.Get(hForkOffset),
			subOffsetRaw:  subOffRaw, subOffsetSet: subOffRaw != "",
			contentType: ct, // "" = inherit source
			ttl:         ttl, ttlSet: ttlSet, expiresAt: expires, expiresSet: expiresSet,
			closed: closed, body: body,
		})
	} else {
		if ct == "" {
			ct = defaultContentType
		}
		res = s.createStream(createParams{
			path: path, contentType: ct,
			ttl: ttl, ttlSet: ttlSet, expiresAt: expires, expiresSet: expiresSet,
			closed: closed, body: body,
		})
	}
	if res.status == http.StatusNotFound {
		http.Error(w, "source stream not found", http.StatusNotFound)
		return
	}
	if res.status == http.StatusBadRequest {
		http.Error(w, res.msg, http.StatusBadRequest)
		return
	}
	if res.status == http.StatusConflict {
		http.Error(w, "stream exists with different configuration", http.StatusConflict)
		return
	}
	if res.status >= 500 {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set(hContentType, res.contentType)
	h.Set(hStreamNext, res.nextOffset.String())
	if res.closed {
		h.Set(hStreamClosed, "true")
	}
	if res.status == http.StatusCreated {
		h.Set(hLocation, absoluteURL(r, path))
	}
	w.WriteHeader(res.status)
}

type createParams struct {
	path        string
	contentType string
	ttl         uint64
	ttlSet      bool
	expiresAt   int64
	expiresSet  bool
	closed      bool
	body        []byte
}

type createResult struct {
	status      int
	msg         string
	contentType string
	nextOffset  Offset
	closed      bool
}

func (s *Server) createStream(p createParams) createResult {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()

	// A soft-deleted path is blocked from re-creation.
	if reason, _, found, err := s.loadTombstone(p.path); err != nil {
		return createResult{status: 500}
	} else if found && reason == tombSoftDelete {
		return createResult{status: http.StatusConflict}
	}

	// Idempotency against an existing stream.
	if meta, ok, err := s.loadMeta(p.path); err != nil {
		return createResult{status: 500}
	} else if ok {
		reqTTL, reqExpires := uint64(0), int64(0)
		if p.ttlSet {
			reqTTL = p.ttl
		}
		if p.expiresSet {
			reqExpires = p.expiresAt
		}
		matches := contentTypeMatches(meta.ContentType, p.contentType) &&
			meta.TTLSeconds == reqTTL && meta.ExpiresAt == reqExpires &&
			meta.Closed == p.closed
		if !matches {
			return createResult{status: http.StatusConflict}
		}
		tailSeq, err := s.loadTailSeq(meta.StreamID)
		if err != nil {
			return createResult{status: 500}
		}
		return createResult{status: http.StatusOK, contentType: meta.ContentType, nextOffset: tailOffset(tailSeq), closed: meta.Closed}
	}

	// New stream. Derive initial records from the body.
	var records [][]byte
	if len(p.body) > 0 {
		if isJSONStream(p.contentType) {
			recs, err := flattenJSON(p.body)
			switch {
			case errors.Is(err, errEmptyArray):
				// empty array on PUT is a valid empty stream
			case err != nil:
				return createResult{status: http.StatusBadRequest, msg: "invalid JSON"}
			default:
				records = recs
			}
		} else {
			records = [][]byte{p.body}
		}
	}

	id := s.nextID
	now := time.Now()
	meta := &StreamMeta{
		StreamID:    id,
		ContentType: p.contentType,
		TTLSeconds:  boolU64(p.ttlSet, p.ttl),
		ExpiresAt:   boolI64(p.expiresSet, p.expiresAt),
		Closed:      p.closed,
		CreatedAtMs: now.UnixMilli(),
	}
	// Establish the initial expiry deadline (SPEC §10). Sliding TTL counts from
	// now; Stream-Expires-At is absolute. The +1 on TTL guards against
	// sub-second truncation of now so the stream never expires before now+TTL.
	if p.ttlSet {
		meta.Deadline = uint64(now.Unix()) + p.ttl + 1
	} else if p.expiresSet && p.expiresAt > 0 {
		meta.Deadline = uint64(p.expiresAt)
	}
	n := uint64(len(records))

	ops := make([]Op, 0, len(records)+5)
	for i, rec := range records {
		ops = append(ops, Op{Key: recordKey(id, uint64(i)), Val: marshalRecord(recFlagNone, rec)})
	}
	ops = append(ops,
		Op{Key: metaKey(p.path), Val: meta.Marshal()},
		Op{Key: idMappingKey(id), Val: []byte(p.path)},
		Op{Key: tailKey(id), Val: marshalTail(n, now.Unix())},
		Op{Key: idCounterKey(), Val: u64Value(id + 1)},
	)
	if meta.Deadline > 0 {
		ops = append(ops, Op{Key: expiryKey(meta.Deadline, id), Val: u64Value(boolU64(p.ttlSet, p.ttl))})
	}
	if err := s.store.Commit(ops, true); err != nil {
		return createResult{status: 500}
	}
	s.nextID = id + 1

	return createResult{status: http.StatusCreated, contentType: p.contentType, nextOffset: tailOffset(n), closed: p.closed}
}

// --- POST: append / close ---

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request, path string) {
	// Parse producer headers (all-or-none).
	producer, perr := parseProducer(r)
	if perr != nil {
		http.Error(w, perr.Error(), http.StatusBadRequest)
		return
	}

	streamSeq := headerPtr(r.Header.Get(hStreamSeq))
	closeStream := headerTrue(r.Header.Get(hStreamClosed))
	reqCT := r.Header.Get(hContentType)

	body, tooLarge, err := readBody(w, r, s.cfg.MaxRecordBytes)
	if err != nil {
		if tooLarge {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	hasBody := len(body) > 0

	// Empty body is only valid for a close-only request.
	if !hasBody && !closeStream {
		http.Error(w, "empty body requires Stream-Closed: true", http.StatusBadRequest)
		return
	}
	// A body requires Content-Type (protocol §5.2); it MAY be omitted only when
	// the body is empty (close-only).
	if hasBody && strings.TrimSpace(reqCT) == "" {
		http.Error(w, "Content-Type is required when a body is provided", http.StatusBadRequest)
		return
	}

	// Soft-deleted streams are 410 for all direct operations.
	if reason, _, found, err := s.loadTombstone(path); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	} else if found && reason == tombSoftDelete {
		http.Error(w, "stream gone", http.StatusGone)
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

	// Flatten the body into records only when it will actually be appended:
	// the stream must be open and the content type must match. Closed/mismatch
	// cases are resolved authoritatively by the streamer (correct precedence).
	var records [][]byte
	if hasBody && !st.closed.Load() && contentTypeMatches(st.contentType, reqCT) {
		if st.isJSON {
			recs, ferr := flattenJSON(body)
			switch {
			case errors.Is(ferr, errEmptyArray):
				http.Error(w, "empty JSON array", http.StatusBadRequest)
				return
			case ferr != nil:
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			default:
				records = recs
			}
		} else {
			records = [][]byte{body}
		}
	}

	resp, ok := s.submitAppend(path, appendReq{
		records:     records,
		hasBody:     hasBody,
		contentType: reqCT,
		closeStream: closeStream,
		streamSeq:   streamSeq,
		producer:    producer,
	})
	if !ok {
		http.Error(w, "stream not found", http.StatusNotFound)
		return
	}
	s.writeAppendResponse(w, resp)
}

func (s *Server) writeAppendResponse(w http.ResponseWriter, resp appendResp) {
	h := w.Header()
	switch resp.status {
	case http.StatusOK, http.StatusNoContent:
		h.Set(hStreamNext, resp.nextOffset.String())
		if resp.closed {
			h.Set(hStreamClosed, "true")
		}
		if resp.producerEpoch != nil {
			h.Set(hProducerEpoch, strconv.FormatUint(*resp.producerEpoch, 10))
		}
		if resp.producerSeq != nil {
			h.Set(hProducerSeq, strconv.FormatUint(*resp.producerSeq, 10))
		}
		w.WriteHeader(resp.status)
	case http.StatusForbidden:
		if resp.producerEpoch != nil {
			h.Set(hProducerEpoch, strconv.FormatUint(*resp.producerEpoch, 10))
		}
		http.Error(w, "stale producer epoch", http.StatusForbidden)
	case http.StatusConflict:
		if resp.closed {
			h.Set(hStreamClosed, "true")
			h.Set(hStreamNext, resp.nextOffset.String())
		}
		if resp.expectedSeq != nil {
			h.Set(hProducerExpectedSeq, strconv.FormatUint(*resp.expectedSeq, 10))
		}
		if resp.receivedSeq != nil {
			h.Set(hProducerReceivedSeq, strconv.FormatUint(*resp.receivedSeq, 10))
		}
		http.Error(w, "conflict", http.StatusConflict)
	case http.StatusBadRequest:
		http.Error(w, "bad request", http.StatusBadRequest)
	case http.StatusNotFound:
		http.Error(w, "stream not found", http.StatusNotFound)
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// --- HEAD: metadata ---

func (s *Server) handleHead(w http.ResponseWriter, r *http.Request, path string) {
	w.Header().Set(hCacheControl, "no-store")

	if reason, _, found, err := s.loadTombstone(path); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	} else if found && reason == tombSoftDelete {
		w.WriteHeader(http.StatusGone)
		return
	}

	meta, ok, err := s.loadMeta(path)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	tail, closed, _, _, err := s.liveState(path)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set(hContentType, meta.ContentType)
	h.Set(hStreamNext, tailOffset(tail).String())
	if meta.TTLSeconds > 0 {
		h.Set(hStreamTTL, strconv.FormatUint(meta.TTLSeconds, 10))
	}
	if meta.ExpiresAt > 0 {
		h.Set(hStreamExpires, time.Unix(meta.ExpiresAt, 0).UTC().Format(time.RFC3339))
	}
	if closed {
		h.Set(hStreamClosed, "true")
	}
	w.WriteHeader(http.StatusOK)
}

// --- DELETE ---

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, path string) {
	s.metaMu.Lock()

	if reason, _, found, err := s.loadTombstone(path); err != nil {
		s.metaMu.Unlock()
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	} else if found && reason == tombSoftDelete {
		s.metaMu.Unlock()
		http.Error(w, "stream gone", http.StatusGone)
		return
	}

	meta, ok, err := s.loadMeta(path)
	if err != nil {
		s.metaMu.Unlock()
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		s.metaMu.Unlock()
		http.Error(w, "stream not found", http.StatusNotFound)
		return
	}

	// A stream with outstanding forks is soft-deleted (retains data for fork
	// readers, serves 410 on its own path); otherwise it is hard-deleted (SPEC §11).
	var derr error
	if s.loadRefCount(meta.StreamID) > 0 {
		derr = s.softDeleteLocked(path, meta)
	} else {
		derr = s.deleteStreamLocked(path, meta)
	}
	if derr != nil {
		s.metaMu.Unlock()
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.metaMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// --- request parsing helpers ---

func readBody(w http.ResponseWriter, r *http.Request, max int64) (body []byte, tooLarge bool, err error) {
	if r.Body == nil {
		return nil, false, nil
	}
	limited := http.MaxBytesReader(w, r.Body, max)
	b, err := io.ReadAll(limited)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, true, err
		}
		return nil, false, err
	}
	return b, false, nil
}

// parseExpiry parses Stream-TTL and Stream-Expires-At. They are mutually
// exclusive (protocol §5.1). ok is false on any malformed / conflicting value.
func parseExpiry(r *http.Request) (ttl uint64, ttlSet bool, expires int64, expiresSet bool, ok bool) {
	if v := r.Header.Get(hStreamTTL); v != "" {
		n, valid := parseStrictUint(v)
		if !valid {
			return 0, false, 0, false, false
		}
		ttl, ttlSet = n, true
	}
	if v := r.Header.Get(hStreamExpires); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return 0, false, 0, false, false
		}
		expires, expiresSet = t.Unix(), true
	}
	if ttlSet && expiresSet {
		return 0, false, 0, false, false
	}
	return ttl, ttlSet, expires, expiresSet, true
}

// parseProducer parses the Producer-* headers. All three must be present or
// none. Returns nil,nil when none are present.
func parseProducer(r *http.Request) (*producerHeaders, error) {
	id := r.Header.Get(hProducerID)
	epochStr := r.Header.Get(hProducerEpoch)
	seqStr := r.Header.Get(hProducerSeq)
	present := 0
	if id != "" {
		present++
	}
	if epochStr != "" {
		present++
	}
	if seqStr != "" {
		present++
	}
	if present == 0 {
		return nil, nil
	}
	if present != 3 || id == "" {
		return nil, errors.New("incomplete producer headers")
	}
	epoch, ok := parseProducerInt(epochStr)
	if !ok {
		return nil, errors.New("invalid Producer-Epoch")
	}
	seq, ok := parseProducerInt(seqStr)
	if !ok {
		return nil, errors.New("invalid Producer-Seq")
	}
	return &producerHeaders{id: id, epoch: epoch, seq: seq}, nil
}

// headerTrue implements the Stream-Closed presence rule (protocol §4.1): only
// the exact value "true" (case-insensitive) counts.
func headerTrue(v string) bool { return strings.EqualFold(v, "true") }

func headerPtr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// parseStrictUint accepts only canonical non-negative decimals: no leading
// zeros (except "0"), sign, decimal point, or exponent (protocol §5.1).
func parseStrictUint(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

const maxJSInt = uint64(1)<<53 - 1

// parseProducerInt parses a non-negative integer <= 2^53-1 (protocol §5.2.1).
func parseProducerInt(s string) (uint64, bool) {
	n, ok := parseStrictUint(s)
	if !ok || n > maxJSInt {
		return 0, false
	}
	return n, true
}

func boolU64(set bool, v uint64) uint64 {
	if set {
		return v
	}
	return 0
}

func boolI64(set bool, v int64) int64 {
	if set {
		return v
	}
	return 0
}
