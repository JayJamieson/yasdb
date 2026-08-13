package yasdb

import (
	"context"
	"sync"
)

// WriterOptions configures Stream.Writer. The zero value is valid.
type WriterOptions struct {
	// Concurrency bounds how many Append calls are in flight at once —
	// what lets a single logical producer (one goroutine calling Write in
	// a loop) get the engine's group-commit batching, which otherwise
	// only kicks in across genuinely concurrent callers (DESIGN.md), and
	// what Write blocks against once exceeded (the backpressure window).
	// <= 0 defaults to 32; tune from BENCHMARKS.md's fan-out numbers for
	// your durability mode and record size.
	Concurrency int
}

// Writer is a high-throughput, backpressured ingestion path into one
// stream. Each Write call acquires one of Concurrency slots — blocking
// (backpressure) once they're all in use — and appends in its own
// goroutine, so a single producer gets the same concurrent-Append
// batching the engine already does for genuinely concurrent callers. It
// exists for a single logical producer with its own pace to manage —
// e.g. a goroutine draining a Kafka consumer — as opposed to
// Stream.Append's one-off call, the natural fit for a request handler on
// a CRUD/REST route.
//
// Unlike Stream.Chunks on the read side, blocking the caller here is
// deliberate: Write blocks once Concurrency appends are outstanding, so
// backpressure propagates straight back to whatever is feeding
// Write — slowing exactly the producer overwhelming this one stream,
// without touching any other stream or reader. Since each Write spawns a
// short-lived, self-contained goroutine rather than feeding a long-lived
// worker pool, there is no shared queue to drain or coordinate shutdown
// with, and so no window where an accepted record could end up owned by
// nothing.
type Writer struct {
	s   *Stream
	sem chan struct{}

	wg  sync.WaitGroup
	mu  sync.Mutex
	err error
}

// Writer returns a Writer for s.
func (s *Stream) Writer(opts WriterOptions) *Writer {
	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = 32
	}
	return &Writer{s: s, sem: make(chan struct{}, concurrency)}
}

// Write appends record, blocking (backpressure) until a concurrency slot
// is free or ctx is done. It returns as soon as the append is under way,
// not once it is durable — call Flush (or Close) to wait for every Write
// accepted so far to finish, and to learn whether any of them failed.
//
// It returns immediately, without waiting for a slot, once the writer
// already has a terminal error (e.g. an earlier Append failed with
// ErrStreamClosed): every further write would only fail the same way.
func (w *Writer) Write(ctx context.Context, record []byte) error {
	if err := w.Err(); err != nil {
		return err
	}
	select {
	case w.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer func() { <-w.sem }()
		if _, err := w.s.Append(ctx, record); err != nil {
			w.setErr(err)
		}
	}()
	return nil
}

// Flush blocks until every record Write has accepted so far has finished
// appending, then returns the writer's first error, if any — nil means
// everything accepted before this call is durable. A caller feeding
// Writer from an external source (e.g. a Kafka consumer) that needs to
// checkpoint should do so only after a successful Flush, not after each
// Write, the same way a batching producer client works.
func (w *Writer) Flush(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return w.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close is Flush: a Writer holds no resources of its own beyond the
// goroutines Write starts, and each of those is already gone by the time
// it's counted in Flush's wait. Close exists so Writer reads like the
// rest of this package's resources, and so a deferred close reads
// naturally at the call site.
func (w *Writer) Close(ctx context.Context) error { return w.Flush(ctx) }

// Err returns the writer's first error, if any, without blocking.
func (w *Writer) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *Writer) setErr(err error) {
	w.mu.Lock()
	if w.err == nil {
		w.err = err
	}
	w.mu.Unlock()
}
