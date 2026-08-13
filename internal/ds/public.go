package ds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/JayJamieson/yasdb/internal/state"
)

// This file is the embeddable Go surface for the engine: everything a
// same-module in-process caller needs (github.com/JayJamieson/yasdb/yasdb
// is a thin wrap over it), reached without net/http at all. See
// docs/rfcs/0002-embeddable-library-api.md. It intentionally wraps
// already-tested internal methods (submitAppend, createStream, readRange,
// liveState, createFork, getOrSpawn) rather than reimplementing any engine
// logic: the risk profile is low because what runs underneath does not
// change.

// Sentinel errors returned by this file's methods. They are the typed
// replacement for the HTTP status codes handlers.go writes: a caller
// switches on errors.Is instead of an int.
var (
	ErrNotFound               = errors.New("yasdb: stream not found")
	ErrGone                   = errors.New("yasdb: stream soft-deleted")
	ErrConflict               = errors.New("yasdb: stream exists with different configuration")
	ErrStreamClosed           = errors.New("yasdb: stream is closed")
	ErrBadRequest             = errors.New("yasdb: bad request")
	ErrNotStateProtocolStream = errors.New("yasdb: stream is not a State Protocol JSON message stream")
)

// --- stream metadata / state ---

// StreamInfo is a stream's current metadata and live state — the in-process
// equivalent of what a HEAD request returns.
type StreamInfo struct {
	StreamID    uint64
	ContentType string
	IsJSON      bool
	Closed      bool
	Tail        uint64
	ForkedFrom  uint64 // 0 = not a fork
	ForkOffset  uint64
	TTLSeconds  uint64
	ExpiresAt   int64
}

// Info returns path's current metadata and live tail/closed state. It
// returns ErrNotFound if the stream was never created (or hard-deleted),
// and ErrGone if it was soft-deleted (still referenced by a fork).
func (s *Server) Info(path string) (StreamInfo, error) {
	if reason, _, found, err := s.loadTombstone(path); err != nil {
		return StreamInfo{}, err
	} else if found && reason == tombSoftDelete {
		return StreamInfo{}, ErrGone
	}
	meta, ok, err := s.loadMeta(path)
	if err != nil {
		return StreamInfo{}, err
	}
	if !ok {
		return StreamInfo{}, ErrNotFound
	}
	tail, closed, _, _, err := s.liveState(path)
	if err != nil {
		return StreamInfo{}, err
	}
	return StreamInfo{
		StreamID:    meta.StreamID,
		ContentType: meta.ContentType,
		IsJSON:      isJSONStream(meta.ContentType),
		Closed:      closed,
		Tail:        tail,
		ForkedFrom:  meta.ForkedFrom,
		ForkOffset:  meta.ForkOffset,
		TTLSeconds:  meta.TTLSeconds,
		ExpiresAt:   meta.ExpiresAt,
	}, nil
}

// --- create / fork / delete ---

// CreateStream creates path as a new stream with the given content type
// (protocol PUT, no initial body). It is idempotent: calling it again with
// the same content type on an existing stream returns its current tail
// offset rather than erroring; a mismatched content type returns
// ErrConflict.
func (s *Server) CreateStream(path, contentType string) (Offset, error) {
	if contentType == "" {
		contentType = defaultContentType
	}
	return createResultToOffset(s.createStream(createParams{path: path, contentType: contentType}))
}

// CreateFork creates path as a fork of sourcePath, diverging at offset at.
// It inherits sourcePath's content type. Idempotent the same way
// CreateStream is.
func (s *Server) CreateFork(path, sourcePath string, at Offset) (Offset, error) {
	return createResultToOffset(s.createFork(forkParams{
		path: path, sourcePath: sourcePath, forkOffsetRaw: at.String(),
	}))
}

func createResultToOffset(res createResult) (Offset, error) {
	switch res.status {
	case http.StatusCreated, http.StatusOK:
		return res.nextOffset, nil
	case http.StatusConflict:
		return Offset{}, ErrConflict
	case http.StatusNotFound:
		return Offset{}, ErrNotFound
	case http.StatusBadRequest:
		return Offset{}, fmt.Errorf("%w: %s", ErrBadRequest, res.msg)
	default:
		return Offset{}, errors.New("internal error")
	}
}

// DeleteStream deletes path (protocol DELETE): hard-deleted if nothing
// forks it, soft-deleted (data retained for fork readers, path itself
// returns ErrGone from then on) otherwise. Returns ErrNotFound if the path
// was never created, ErrGone if it is already soft-deleted.
func (s *Server) DeleteStream(path string) error {
	return s.deleteStream(path)
}

// --- append / close ---

// Append appends records to path, in order, as one durable batch, and
// returns the offset immediately after the last one written. Returns
// ErrNotFound if the stream does not exist, ErrGone if soft-deleted, and
// ErrStreamClosed if the stream has already been closed.
func (s *Server) Append(path string, records [][]byte) (Offset, error) {
	return s.appendOrClose(path, records, false)
}

// CloseStream marks path closed (EOF, protocol Stream-Closed: true). It is
// idempotent: closing an already-closed stream just returns its current
// tail.
func (s *Server) CloseStream(path string) (Offset, error) {
	return s.appendOrClose(path, nil, true)
}

// appendOrClose is Append/CloseStream's shared path. The content type is
// resolved from the stream's own resident streamer rather than taken from
// the caller, so — unlike the HTTP surface, where Content-Type is a client
// header that can be wrong — an in-process append can never fail content
// type validation against the stream it just looked up.
func (s *Server) appendOrClose(path string, records [][]byte, closeStream bool) (Offset, error) {
	st, found := s.registry.get(path)
	if !found {
		if reason, _, tombFound, err := s.loadTombstone(path); err != nil {
			return Offset{}, err
		} else if tombFound && reason == tombSoftDelete {
			return Offset{}, ErrGone
		}
		var err error
		st, found, err = s.getOrSpawn(path)
		if err != nil {
			return Offset{}, err
		}
	}
	if !found {
		return Offset{}, ErrNotFound
	}

	var ct string
	if len(records) > 0 {
		ct = st.contentType
	}
	resp, ok := s.submitAppend(path, appendReq{
		records: records, hasBody: len(records) > 0, contentType: ct, closeStream: closeStream,
	}, st)
	if !ok {
		return Offset{}, ErrNotFound
	}
	return appendRespToOffset(resp)
}

func appendRespToOffset(resp appendResp) (Offset, error) {
	switch resp.status {
	case http.StatusOK, http.StatusNoContent:
		return resp.nextOffset, nil
	case http.StatusConflict:
		return resp.nextOffset, ErrStreamClosed
	case http.StatusNotFound:
		return Offset{}, ErrNotFound
	case http.StatusBadRequest:
		return Offset{}, ErrBadRequest
	default:
		if resp.err != nil {
			return Offset{}, resp.err
		}
		return Offset{}, fmt.Errorf("append failed: status %d", resp.status)
	}
}

// --- catch-up read ---

// Cursor is a bounded catch-up scan over a stream, from Server.Read. It
// wraps readRange — the same assembly HTTP catch-up GETs use — and does no
// I/O until Next is called.
type Cursor struct {
	srv        *Server
	path       string
	id         uint64
	forkedFrom uint64
	forkOffset uint64
	isJSON     bool
	start      Offset
}

// Read returns a Cursor starting at from. Returns ErrNotFound/ErrGone the
// same way Info does.
func (s *Server) Read(path string, from Offset) (*Cursor, error) {
	info, err := s.Info(path)
	if err != nil {
		return nil, err
	}
	return &Cursor{
		srv: s, path: path, id: info.StreamID,
		forkedFrom: info.ForkedFrom, forkOffset: info.ForkOffset,
		isJSON: info.IsJSON, start: from,
	}, nil
}

// Next returns the cursor's next chunk: body is one or more whole JSON
// messages re-wrapped as a single array for a JSON stream, or a raw byte
// span (possibly a partial record, delivered across calls via Offset.Byte)
// for any other content type — the same unit an HTTP catch-up GET returns.
// more is true when the cursor made progress and there may be further data
// available now; it is false once the cursor has reached the stream's
// current tail (readRange's upToDate). Next never blocks; use Server.Tail
// once caught up.
func (c *Cursor) Next(ctx context.Context) (body []byte, next Offset, more bool, err error) {
	if err := ctx.Err(); err != nil {
		return nil, c.start, false, err
	}
	tail, _, _, _, err := c.srv.liveState(c.path)
	if err != nil {
		return nil, c.start, false, err
	}
	body, next, upToDate, err := c.srv.readRange(c.id, c.forkedFrom, c.forkOffset, c.isJSON, c.start, tail, c.srv.cfg.MaxReadBytes)
	if err != nil {
		return nil, c.start, false, err
	}
	c.start = next
	return body, next, !upToDate, nil
}

// --- live tail ---

// LiveCursor is a live, blocking read over a stream, from Server.Tail. It
// wraps liveState + streamer.waiterChan: the same wakeup mechanism the
// HTTP long-poll/SSE readers use, so an Append to this stream from any
// caller — in-process or over HTTP — wakes a parked Next the same way.
type LiveCursor struct {
	srv    *Server
	st     *streamer
	isJSON bool
	start  Offset
}

// Tail returns a LiveCursor starting at from. Returns ErrNotFound/ErrGone
// the same way Info does.
func (s *Server) Tail(path string, from Offset) (*LiveCursor, error) {
	st, found, err := s.getOrSpawn(path)
	if err != nil {
		return nil, err
	}
	if !found {
		if reason, _, tombFound, terr := s.loadTombstone(path); terr != nil {
			return nil, terr
		} else if tombFound && reason == tombSoftDelete {
			return nil, ErrGone
		}
		return nil, ErrNotFound
	}
	return &LiveCursor{srv: s, st: st, isJSON: st.isJSON, start: from}, nil
}

// Next blocks until data is available, the stream is deleted, or ctx is
// done. It returns ErrStreamClosed once the cursor has delivered every
// record up to a closed stream's tail (there will never be more). While
// parked, it counts as a long-poll reader (streamer.longPollReaders), the
// same accounting the HTTP long-poll surface uses, so a write to this
// stream always wakes it: applyReader only bothers waking parked readers
// when totalReaders() > 0.
func (c *LiveCursor) Next(ctx context.Context) (body []byte, next Offset, err error) {
	st := c.st
	st.longPollReaders.Add(1)
	defer st.longPollReaders.Add(-1)

	for {
		wc := st.waiterChan()
		tail := st.tail.Load()
		closed := st.closed.Load()

		body, next, _, ok := st.cacheRead(nil, c.isJSON, c.start, tail, c.srv.cfg.MaxReadBytes)
		if !ok {
			var rerr error
			body, next, _, rerr = c.srv.readRange(st.id, st.forkedFrom, st.forkOffset, c.isJSON, c.start, tail, c.srv.cfg.MaxReadBytes)
			if rerr != nil {
				return nil, c.start, rerr
			}
		}
		if st.stopped() {
			return nil, c.start, ErrNotFound
		}
		if next != c.start {
			c.start = next
			return body, next, nil
		}
		if closed {
			return nil, c.start, ErrStreamClosed
		}
		select {
		case <-wc:
			continue
		case <-st.stop:
			return nil, c.start, ErrNotFound
		case <-ctx.Done():
			return nil, c.start, ctx.Err()
		}
	}
}

// --- State Protocol ---

// Materialize replays path's full history through a fresh
// state.Materializer and returns it, plus the offset to resume live
// tailing from (via TailState). Returns ErrNotStateProtocolStream if the
// stream is not application/json State Protocol messages; a malformed
// individual message inside an otherwise-valid stream is skipped, not
// fatal (state.Materializer.ApplyAll).
func (s *Server) Materialize(ctx context.Context, path string) (*state.Materializer, Offset, error) {
	cur, err := s.Read(path, Offset{})
	if err != nil {
		return nil, Offset{}, err
	}
	mat := state.NewMaterializer()
	var last Offset
	for {
		body, next, more, err := cur.Next(ctx)
		if err != nil {
			return nil, Offset{}, err
		}
		if msgs, err := decodeStateMessages(body); err != nil {
			return nil, Offset{}, err
		} else if msgs != nil {
			mat.ApplyAll(msgs)
		}
		last = next
		if !more {
			break
		}
	}
	return mat, last, nil
}

// StateCursor is a live, one-message-at-a-time read of a State Protocol
// stream, from Server.TailState. It wraps a LiveCursor, unwrapping each
// delivered chunk's JSON message array.
type StateCursor struct {
	lc  *LiveCursor
	buf []state.Message
}

// TailState returns a StateCursor starting at from (see Materialize's
// returned offset to resume from the current materialized state).
func (s *Server) TailState(path string, from Offset) (*StateCursor, error) {
	lc, err := s.Tail(path, from)
	if err != nil {
		return nil, err
	}
	return &StateCursor{lc: lc}, nil
}

// Next returns the next State Protocol message, blocking the same way
// LiveCursor.Next does. Returns ErrNotStateProtocolStream if a delivered
// chunk is not a JSON message array.
func (c *StateCursor) Next(ctx context.Context) (state.Message, error) {
	for len(c.buf) == 0 {
		body, _, err := c.lc.Next(ctx)
		if err != nil {
			return state.Message{}, err
		}
		msgs, err := decodeStateMessages(body)
		if err != nil {
			return state.Message{}, err
		}
		c.buf = msgs
	}
	m := c.buf[0]
	c.buf = c.buf[1:]
	return m, nil
}

// decodeStateMessages unmarshals a chunk body (a JSON array, per json.go's
// wrapJSONArray) into its individual State Protocol messages. An empty
// body decodes to (nil, nil): no messages, not an error.
func decodeStateMessages(body []byte) ([]state.Message, error) {
	if len(body) == 0 {
		return nil, nil
	}
	var msgs []state.Message
	if err := json.Unmarshal(body, &msgs); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotStateProtocolStream, err)
	}
	return msgs, nil
}
