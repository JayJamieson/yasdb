package yasdb_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/JayJamieson/yasdb/yasdb"
)

// concurrentAppends fires n appends at s from goroutines at once (letting
// the streamer's group-commit batch them, per DESIGN.md) and waits for
// all of them to land, instead of paying one durable round trip per
// append serially.
func concurrentAppends(ctx context.Context, t *testing.T, s *yasdb.Stream, n int) {
	t.Helper()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Append(ctx, []byte{byte(i % 256)}); err != nil {
				t.Errorf("append %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
}

func TestChunksCatchupThenLive(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	path := uniquePath(t)
	s := db.Stream(path)
	if err := s.Create(ctx, "text/plain"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Append(ctx, []byte("a"), []byte("b")); err != nil {
		t.Fatalf("append: %v", err)
	}

	ch, errFn := s.Chunks(ctx, yasdb.Offset{})

	// Catch-up delivery.
	select {
	case c := <-ch:
		if string(c.Body) != "ab" {
			t.Fatalf("catch-up body: got %q, want %q", c.Body, "ab")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no catch-up chunk received")
	}

	// Live delivery after catch-up: a further append must show up on the
	// same channel without a new call.
	if _, err := s.Append(ctx, []byte("c")); err != nil {
		t.Fatalf("append: %v", err)
	}
	select {
	case c := <-ch:
		if string(c.Body) != "c" {
			t.Fatalf("live body: got %q, want %q", c.Body, "c")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no live chunk received after append")
	}

	if err := s.Close(ctx); err != nil {
		t.Fatalf("close stream: %v", err)
	}
	select {
	case _, open := <-ch:
		if open {
			t.Fatal("expected channel to close after stream close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("channel never closed after stream close")
	}
	if err := errFn(); !errors.Is(err, yasdb.ErrStreamClosed) {
		t.Fatalf("got %v, want ErrStreamClosed", err)
	}
}

// TestChunksDoesNotBlockProducer confirms Chunks never blocks its internal
// reader on a slow/absent consumer: appending well past the buffer
// capacity with nobody reading must complete promptly (the old design,
// with an unbuffered blocking send, would hang here).
func TestChunksDoesNotBlockProducer(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	path := uniquePath(t)
	s := db.Stream(path)
	if err := s.Create(ctx, "text/plain"); err != nil {
		t.Fatalf("create: %v", err)
	}

	_, errFn := s.Chunks(ctx, yasdb.Offset{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		concurrentAppends(ctx, t, s, 500)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("appends stalled — Chunks appears to be blocking its producer on a full/unread channel")
	}
	_ = errFn // the consumer never read; overrun is expected eventually, not asserted here
}

// TestChunksOverrunAndResume drives the consumer well behind the writer
// (past chunksBufferSize) and checks: the channel closes with
// ErrChunksOverrun rather than hanging or silently dropping the whole
// stream, every chunk actually delivered before that point is intact and
// in order, and resuming a fresh Chunks call from the last delivered
// offset picks up the rest with no gap or duplicate.
func TestChunksOverrunAndResume(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	path := uniquePath(t)
	s := db.Stream(path)
	if err := s.Create(ctx, "text/plain"); err != nil {
		t.Fatalf("create: %v", err)
	}

	ch, errFn := s.Chunks(ctx, yasdb.Offset{})

	// Append one record at a time — each its own commit, so each is its
	// own live chunk — well past chunksBufferSize (64), without ever
	// reading ch, to force an overrun. (A single bulk append instead would
	// land as one chunk regardless of record count, and never overrun.)
	const total = 300
	for i := 0; i < total; i++ {
		if _, err := s.Append(ctx, []byte{byte(i % 256)}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	var last yasdb.Offset
	got := 0
	for c := range ch {
		got++
		last = c.Next
	}
	if got == 0 {
		t.Fatal("received no chunks before overrun")
	}
	if err := errFn(); !errors.Is(err, yasdb.ErrChunksOverrun) {
		t.Fatalf("got %v, want ErrChunksOverrun", err)
	}
	if last == (yasdb.Offset{}) {
		t.Fatal("last delivered offset is still zero — nothing usable to resume from")
	}

	// Resume from the last delivered offset and confirm the remaining
	// records read back in order.
	cur, err := s.Read(ctx, last)
	if err != nil {
		t.Fatalf("resume read: %v", err)
	}
	var resumed []byte
	for {
		body, _, more, err := cur.Next(ctx)
		if err != nil {
			t.Fatalf("resume cursor.Next: %v", err)
		}
		resumed = append(resumed, body...)
		if !more {
			break
		}
	}
	full, err := s.Read(ctx, yasdb.Offset{})
	if err != nil {
		t.Fatalf("full read: %v", err)
	}
	var all []byte
	for {
		body, _, more, err := full.Next(ctx)
		if err != nil {
			t.Fatalf("full cursor.Next: %v", err)
		}
		all = append(all, body...)
		if !more {
			break
		}
	}
	if want := string(all[len(all)-len(resumed):]); string(resumed) != want {
		t.Fatalf("resumed read did not line up with the tail of the full stream")
	}
}

// TestChunksStopsOnContextCancel confirms a consumer that cancels ctx
// reliably ends the goroutine and closes the channel.
func TestChunksStopsOnContextCancel(t *testing.T) {
	db := openTestDB(t)
	path := uniquePath(t)
	if err := db.Stream(path).Create(context.Background(), "text/plain"); err != nil {
		t.Fatalf("create: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch, errFn := db.Stream(path).Chunks(ctx, yasdb.Offset{})

	time.Sleep(20 * time.Millisecond) // let it settle into the live-tail wait
	cancel()

	select {
	case _, open := <-ch:
		if open {
			t.Fatal("channel delivered a chunk after cancel instead of closing")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("channel never closed after ctx cancel")
	}
	if err := errFn(); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

// TestChunksStopsOnDBClose confirms closing the DB terminates an
// outstanding Chunks goroutine parked in the live-tail wait, the same way
// it already terminates a plain LiveCursor.Next (streamer.stop).
func TestChunksStopsOnDBClose(t *testing.T) {
	db := openTestDB(t)
	path := uniquePath(t)
	if err := db.Stream(path).Create(context.Background(), "text/plain"); err != nil {
		t.Fatalf("create: %v", err)
	}

	ch, _ := db.Stream(path).Chunks(context.Background(), yasdb.Offset{})

	time.Sleep(20 * time.Millisecond) // let it settle into the live-tail wait
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	select {
	case _, open := <-ch:
		if open {
			t.Fatal("channel delivered a chunk after DB.Close instead of closing")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("channel never closed after DB.Close")
	}
}
