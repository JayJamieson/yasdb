package yasdb_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JayJamieson/yasdb/yasdb"
)

var pathCounter atomic.Uint64

// uniquePath keeps each test's stream on its own path within a shared
// in-memory store, so tests can run with -parallel without colliding.
func uniquePath(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("/%s/%d", t.Name(), pathCounter.Add(1))
}

func openTestDB(t *testing.T) *yasdb.DB {
	t.Helper()
	// notifier durability + a 1ms flush: the default sync/100ms-flush combo
	// (this project's engine default) bounds one durable append at ~1 per
	// flush interval, which is fine for the low-volume tests elsewhere in
	// this package but far too slow for tests that append in bulk.
	store, err := yasdb.OpenStore(fmt.Sprintf("yasdb-test-%d", pathCounter.Add(1)), "memory:///", yasdb.StoreTuning{FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	db, err := yasdb.Open(store, yasdb.Config{Durability: "notifier", NotifierPollInterval: time.Millisecond})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestCreateAppendRead(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	path := uniquePath(t)
	s := db.Stream(path)

	if err := s.Create(ctx, "application/json"); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Idempotent re-create with the same content type must succeed.
	if err := s.Create(ctx, "application/json"); err != nil {
		t.Fatalf("idempotent re-create: %v", err)
	}
	// A different content type on the same path must conflict.
	if err := s.Create(ctx, "text/plain"); !errors.Is(err, yasdb.ErrConflict) {
		t.Fatalf("re-create with different content type: got %v, want ErrConflict", err)
	}

	if _, err := s.AppendJSON(ctx, map[string]int{"id": 1}, map[string]int{"id": 2}); err != nil {
		t.Fatalf("append: %v", err)
	}

	cur, err := s.Read(ctx, yasdb.Offset{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body, _, more, err := cur.Next(ctx)
	if err != nil {
		t.Fatalf("cursor.Next: %v", err)
	}
	if more {
		t.Fatalf("more=true after a single small batch, want caught up")
	}
	var got []map[string]int
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal body %q: %v", body, err)
	}
	want := []map[string]int{{"id": 1}, {"id": 2}}
	if len(got) != len(want) || got[0]["id"] != 1 || got[1]["id"] != 2 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAppendNotFound(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	s := db.Stream(uniquePath(t))
	if _, err := s.Append(ctx, []byte("x")); !errors.Is(err, yasdb.ErrNotFound) {
		t.Fatalf("append to missing stream: got %v, want ErrNotFound", err)
	}
}

func TestCloseThenAppendRejected(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	s := db.Stream(uniquePath(t))
	if err := s.Create(ctx, "text/plain"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Closing twice is idempotent.
	if err := s.Close(ctx); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := s.Append(ctx, []byte("late")); !errors.Is(err, yasdb.ErrStreamClosed) {
		t.Fatalf("append after close: got %v, want ErrStreamClosed", err)
	}
}

func TestDeleteThenGone(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	path := uniquePath(t)
	s := db.Stream(path)
	if err := s.Create(ctx, "text/plain"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Delete(ctx); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.Delete(ctx); !errors.Is(err, yasdb.ErrNotFound) {
		t.Fatalf("delete again: got %v, want ErrNotFound (hard-deleted, no fork refs)", err)
	}
	// A hard-deleted path is immediately recreatable (DESIGN.md).
	if err := s.Create(ctx, "text/plain"); err != nil {
		t.Fatalf("recreate after delete: %v", err)
	}
}

func TestFork(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	src := db.Stream(uniquePath(t))
	if err := src.Create(ctx, "text/plain"); err != nil {
		t.Fatalf("create source: %v", err)
	}
	tail, err := src.Append(ctx, []byte("a"), []byte("b"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	fork, err := src.Fork(ctx, uniquePath(t), tail)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if _, err := fork.Append(ctx, []byte("c")); err != nil {
		t.Fatalf("append to fork: %v", err)
	}

	cur, err := fork.Read(ctx, yasdb.Offset{})
	if err != nil {
		t.Fatalf("read fork: %v", err)
	}
	body, _, _, err := cur.Next(ctx)
	if err != nil {
		t.Fatalf("cursor.Next: %v", err)
	}
	if string(body) != "abc" {
		t.Fatalf("fork read: got %q, want %q (inherited a+b, own c)", body, "abc")
	}
}

// TestTailWakesOnAppend is the key correctness property from RFC 0002: a
// write through Stream.Append must wake a LiveCursor.Next parked in
// Stream.Tail, through the same streamer.waiterChan/applyReader path the
// HTTP long-poll/SSE readers use. If LiveCursor failed to register itself
// as a reader (streamer.longPollReaders), the writer would never call
// Wake() and this would hang until the test's own timeout fires.
func TestTailWakesOnAppend(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	path := uniquePath(t)
	s := db.Stream(path)
	if err := s.Create(ctx, "text/plain"); err != nil {
		t.Fatalf("create: %v", err)
	}

	lc, err := s.Tail(ctx, yasdb.Offset{})
	if err != nil {
		t.Fatalf("tail: %v", err)
	}

	type result struct {
		body []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		body, _, err := lc.Next(ctx)
		done <- result{body, err}
	}()

	// Give Next a moment to actually park on waiterChan before the append,
	// so this exercises the wake path rather than the immediate-data path.
	time.Sleep(20 * time.Millisecond)

	if _, err := s.Append(ctx, []byte("hello")); err != nil {
		t.Fatalf("append: %v", err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("LiveCursor.Next: %v", r.err)
		}
		if string(r.body) != "hello" {
			t.Fatalf("got %q, want %q", r.body, "hello")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LiveCursor.Next never woke after Append — write path is not waking live readers")
	}
}

// TestTailCancelledByContext confirms LiveCursor.Next honors ctx.Done()
// instead of blocking forever (RFC 0002's context-cancellation-replaces-
// long-poll-timeout design decision).
func TestTailCancelledByContext(t *testing.T) {
	db := openTestDB(t)
	path := uniquePath(t)
	if err := db.Stream(path).Create(context.Background(), "text/plain"); err != nil {
		t.Fatalf("create: %v", err)
	}
	lc, err := db.Stream(path).Tail(context.Background(), yasdb.Offset{})
	if err != nil {
		t.Fatalf("tail: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, err = lc.Next(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Next took %s to respect ctx cancellation", elapsed)
	}
}

func TestMaterializeAndTailState(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	path := uniquePath(t)
	s := db.Stream(path)
	if err := s.Create(ctx, "application/json"); err != nil {
		t.Fatalf("create: %v", err)
	}

	insert := map[string]any{
		"type": "user", "key": "u1", "value": map[string]string{"name": "ada"},
		"headers": map[string]string{"operation": "insert"},
	}
	if _, err := s.AppendJSON(ctx, insert); err != nil {
		t.Fatalf("append: %v", err)
	}

	mat, next, err := s.Materialize(ctx)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	snap := mat.Snapshot()
	if _, ok := snap["user"]["u1"]; !ok {
		t.Fatalf("materialized snapshot missing user/u1: %v", snap)
	}

	sc, err := s.TailState(ctx, next)
	if err != nil {
		t.Fatalf("tailstate: %v", err)
	}
	update := map[string]any{
		"type": "user", "key": "u2", "value": map[string]string{"name": "grace"},
		"headers": map[string]string{"operation": "insert"},
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = s.AppendJSON(context.Background(), update)
	}()

	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	msg, err := sc.Next(tctx)
	if err != nil {
		t.Fatalf("statecursor.Next: %v", err)
	}
	if msg.Type != "user" || msg.Key != "u2" {
		t.Fatalf("got %+v, want type=user key=u2", msg)
	}
}

func TestMaterializeNotStateProtocol(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	path := uniquePath(t)
	s := db.Stream(path)
	if err := s.Create(ctx, "application/json"); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A bare JSON string record is valid application/json, but each element
	// of the message array must unmarshal into a state.Message object —
	// this one can't.
	if _, err := s.AppendJSON(ctx, "not a state protocol message"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, _, err := s.Materialize(ctx); !errors.Is(err, yasdb.ErrNotStateProtocolStream) {
		t.Fatalf("got %v, want ErrNotStateProtocolStream", err)
	}
}
