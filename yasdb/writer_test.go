package yasdb_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/JayJamieson/yasdb/yasdb"
)

func TestWriterBasic(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	path := uniquePath(t)
	s := db.Stream(path)
	if err := s.Create(ctx, "text/plain"); err != nil {
		t.Fatalf("create: %v", err)
	}

	w := s.Writer(yasdb.WriterOptions{Concurrency: 8})
	const n = 200
	for i := 0; i < n; i++ {
		if err := w.Write(ctx, []byte{byte(i % 256)}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if err := w.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}

	info, err := s.Info(ctx)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Tail != n {
		t.Fatalf("tail = %d, want %d", info.Tail, n)
	}
}

// slowStorage wraps a real Storage and adds a fixed delay to every
// CommitAsync — the call the notifier-durability committer uses to land a
// burst — so tests can deterministically control how long one Append
// takes, instead of racing against however fast the real store happens to
// be. This is only possible because Storage is a small, externally
// implementable interface (RFC 0002) — the same property that lets an
// embedder swap in their own backend lets a test swap in a slow one.
type slowStorage struct {
	yasdb.Storage
	delay time.Duration
}

func (s *slowStorage) CommitAsync(ops []yasdb.Op) (uint64, error) {
	time.Sleep(s.delay)
	return s.Storage.CommitAsync(ops)
}

func openSlowTestDB(t *testing.T, delay time.Duration) *yasdb.DB {
	t.Helper()
	store, err := yasdb.OpenStore(fmt.Sprintf("yasdb-slow-test-%d", pathCounter.Add(1)), "memory:///", yasdb.StoreTuning{FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	db, err := yasdb.Open(&slowStorage{Storage: store, delay: delay}, yasdb.Config{Durability: "notifier", NotifierPollInterval: time.Millisecond})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestWriterBackpressureSerializes proves Concurrency actually bounds
// in-flight appends — not just that the option exists — by comparing wall
// time to Flush the same N writes at Concurrency=1 (forced fully serial)
// against a higher concurrency (able to overlap), against an artificially
// slowed store so the difference is unmistakable rather than lost in
// noise.
func TestWriterBackpressureSerializes(t *testing.T) {
	const delay = 100 * time.Millisecond
	const n = 4

	db1 := openSlowTestDB(t, delay)
	ctx := context.Background()
	s1 := db1.Stream(uniquePath(t))
	if err := s1.Create(ctx, "text/plain"); err != nil {
		t.Fatalf("create: %v", err)
	}
	w1 := s1.Writer(yasdb.WriterOptions{Concurrency: 1})
	start := time.Now()
	for i := 0; i < n; i++ {
		if err := w1.Write(ctx, []byte{byte(i)}); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := w1.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	serial := time.Since(start)

	db2 := openSlowTestDB(t, delay)
	s2 := db2.Stream(uniquePath(t))
	if err := s2.Create(ctx, "text/plain"); err != nil {
		t.Fatalf("create: %v", err)
	}
	w2 := s2.Writer(yasdb.WriterOptions{Concurrency: n})
	start = time.Now()
	for i := 0; i < n; i++ {
		if err := w2.Write(ctx, []byte{byte(i)}); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := w2.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	parallel := time.Since(start)

	t.Logf("Concurrency=1: %s, Concurrency=%d: %s", serial, n, parallel)
	if serial < time.Duration(n)*delay/2 {
		t.Fatalf("Concurrency=1 took %s, expected roughly serial (>= ~%s) — backpressure isn't bounding concurrency", serial, time.Duration(n)*delay)
	}
	if parallel >= serial {
		t.Fatalf("Concurrency=%d (%s) did not beat Concurrency=1 (%s) — higher concurrency isn't overlapping appends", n, parallel, serial)
	}
}

// TestWriterBlocksOnFullConcurrency confirms Write actually blocks (not
// just "is slower") once Concurrency slots are all in use: with
// Concurrency=1 and one slow append outstanding, a second Write given a
// short deadline must fail with context.DeadlineExceeded rather than
// somehow proceeding.
func TestWriterBlocksOnFullConcurrency(t *testing.T) {
	db := openSlowTestDB(t, 500*time.Millisecond)
	ctx := context.Background()
	s := db.Stream(uniquePath(t))
	if err := s.Create(ctx, "text/plain"); err != nil {
		t.Fatalf("create: %v", err)
	}

	w := s.Writer(yasdb.WriterOptions{Concurrency: 1})
	if err := w.Write(ctx, []byte("first")); err != nil {
		t.Fatalf("first write: %v", err)
	}

	shortCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := w.Write(shortCtx, []byte("second")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second write: got %v, want context.DeadlineExceeded (slot should still be held by the first, slow append)", err)
	}
}

func TestWriterErrorPropagation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	path := uniquePath(t)
	s := db.Stream(path)
	if err := s.Create(ctx, "text/plain"); err != nil {
		t.Fatalf("create: %v", err)
	}

	w := s.Writer(yasdb.WriterOptions{Concurrency: 4})
	if err := w.Write(ctx, []byte("a")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if err := s.Close(ctx); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	if err := w.Write(ctx, []byte("late")); err != nil {
		t.Fatalf("write after stream close should still be accepted (fails async): %v", err)
	}
	if err := w.Flush(ctx); !errors.Is(err, yasdb.ErrStreamClosed) {
		t.Fatalf("flush: got %v, want ErrStreamClosed", err)
	}

	// Once a terminal error is recorded, further writes fail fast without
	// even trying.
	if err := w.Write(ctx, []byte("later")); !errors.Is(err, yasdb.ErrStreamClosed) {
		t.Fatalf("write after terminal error: got %v, want ErrStreamClosed", err)
	}
}
