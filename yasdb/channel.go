package yasdb

import (
	"context"
	"errors"
	"sync"
)

// ErrChunksOverrun is Chunks' terminal error when the consumer falls
// behind: its internal buffer filled because nothing read from the
// channel for too long. Chunks stops rather than either blocking its
// internal reader indefinitely on a full channel or growing the buffer
// without bound to wait for a consumer that may never catch up. The last
// Chunk received off the channel before it closed carries the offset to
// resume Chunks from.
var ErrChunksOverrun = errors.New("yasdb: Chunks consumer fell behind")

// chunksBufferSize is how many chunks Chunks buffers ahead of the
// consumer before treating it as fallen behind (ErrChunksOverrun).
const chunksBufferSize = 64

// Chunk is one item delivered by Stream.Chunks: the same unit
// Cursor.Next/LiveCursor.Next deliver — one or more whole JSON messages
// re-wrapped as an array for a JSON stream, or a raw byte span otherwise.
type Chunk struct {
	Body []byte
	Next Offset
}

// Chunks starts a background goroutine that reads the stream from `from`
// forward — catching up via Read, then following live via Tail — and
// delivers each chunk on the returned channel, in order, until it ends.
//
// The channel is buffered (chunksBufferSize entries) but never blocks the
// goroutine feeding it: a consumer that stops reading, or reads slower
// than data arrives, does not stall the internal reader. If the buffer
// fills, Chunks stops instead of blocking or growing the buffer without
// bound, and the channel closes with ErrChunksOverrun (see the returned
// func). This means a slow consumer can miss data — Chunks does not apply
// backpressure to the write path — so treat it as a best-effort live
// view, and use the resume offset below to catch back up.
//
// Call the returned func after the channel closes (e.g. after a range
// over it ends) for why it ended: ErrNotFound (the stream was deleted, or
// this Stream's DB was closed), ErrStreamClosed (the stream closed and
// was fully drained), ctx.Err() (ctx ended), or ErrChunksOverrun. It is
// always non-nil once the channel is closed — Chunks only ever stops for
// one of these reasons.
//
//	ch, errFn := s.Chunks(ctx, from)
//	var last yasdb.Offset
//	for c := range ch {
//		last = c.Next
//		handle(c.Body)
//	}
//	if err := errFn(); errors.Is(err, yasdb.ErrChunksOverrun) {
//		// resume with s.Chunks(ctx, last)
//	}
func (s *Stream) Chunks(ctx context.Context, from Offset) (<-chan Chunk, func() error) {
	out := make(chan Chunk, chunksBufferSize)
	result := &chunksResult{}
	go runChunks(ctx, s, from, out, result)
	return out, result.get
}

// chunksResult carries runChunks' terminal error to the caller out of
// band from the data channel, so it can always be reported even when the
// channel's buffer is full (the overrun case) rather than needing a spare
// slot reserved for it.
type chunksResult struct {
	mu  sync.Mutex
	err error
}

func (r *chunksResult) set(err error) {
	r.mu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
}

func (r *chunksResult) get() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func runChunks(ctx context.Context, s *Stream, from Offset, out chan<- Chunk, result *chunksResult) {
	defer close(out)

	pos := from
	cur, err := s.Read(ctx, pos)
	if err != nil {
		result.set(err)
		return
	}
	for {
		before := pos
		body, next, more, err := cur.Next(ctx)
		if err != nil {
			result.set(err)
			return
		}
		pos = next
		// Skip a chunk that delivered nothing (e.g. a JSON stream's "[]" at
		// the tail): the offset not advancing is the reliable signal, since
		// an empty-looking body can still be genuine ("[]" is valid JSON,
		// not absence).
		if next != before && !sendChunk(out, Chunk{Body: body, Next: next}, result) {
			return
		}
		if !more {
			break
		}
	}

	lc, err := s.Tail(ctx, pos)
	if err != nil {
		result.set(err)
		return
	}
	for {
		body, next, err := lc.Next(ctx)
		if err != nil {
			result.set(err)
			return
		}
		if !sendChunk(out, Chunk{Body: body, Next: next}, result) {
			return
		}
	}
}

// sendChunk delivers c without ever blocking: if the buffer is full, it
// records ErrChunksOverrun and returns false instead, so runChunks stops
// rather than piling up unbounded work behind a consumer that isn't
// keeping up.
func sendChunk(out chan<- Chunk, c Chunk, result *chunksResult) bool {
	select {
	case out <- c:
		return true
	default:
		result.set(ErrChunksOverrun)
		return false
	}
}
