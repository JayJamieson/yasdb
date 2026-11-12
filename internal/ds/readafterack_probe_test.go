package ds

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestProbeReadAfterAck asserts read-after-acknowledge consistency in each
// durability mode. It is a direct probe, not a linearizability checker:
// after an append is acknowledged at offset N (to any client), a
// strictly-subsequent read must observe at least N records.
//
// This is the regression test for a real bug (fixed 2026-07-29): under
// concurrent pipelining, notifier mode published the reader-visible tail
// out of order, letting a read observe fewer records than an offset
// already acknowledged. The cause was durabilityNotifier.subscribe firing
// an already-durable callback on the caller's goroutine, racing the poller
// firing an earlier burst. The fix routes every callback through the
// single poller goroutine, so applyReader stays ordered.
func TestProbeReadAfterAck(t *testing.T) {
	if testing.Short() {
		t.Skip("read-after-ack probe skipped in -short")
	}
	const (
		producers = 5
		perProd   = 200
	)
	for _, mode := range []string{"sync", "notifier"} {
		t.Run(mode, func(t *testing.T) {
			ts, _ := startServer(t, mustMemStore(t), Config{
				Durability:           mode,
				NotifierPollInterval: time.Millisecond,
				LongPollTimeout:      100 * time.Millisecond,
				MaxReadBytes:         1 << 20,
			})
			resp := do(t, ts, "PUT", "/probe", "", hdr("Content-Type", "application/json"))
			wantStatus(t, resp, 201)
			resp.Body.Close()

			cl := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
			var maxAcked atomic.Int64
			var violations atomic.Int64
			var wg sync.WaitGroup

			for p := 0; p < producers; p++ {
				wg.Add(1)
				go func(pid int) {
					defer wg.Done()
					for seq := 0; seq < perProd; seq++ {
						tok := fmt.Sprintf("p%d-%d", pid, seq)
						r, _ := http.NewRequest("POST", ts.URL+"/probe", strings.NewReader(`["`+tok+`"]`))
						r.Header.Set("Content-Type", "application/json")
						r.Header.Set("Producer-Id", fmt.Sprintf("p%d", pid))
						r.Header.Set("Producer-Epoch", "1")
						r.Header.Set("Producer-Seq", fmt.Sprintf("%d", seq))
						resp, err := cl.Do(r)
						if err != nil {
							t.Errorf("append: %v", err)
							return
						}
						next := resp.Header.Get("Stream-Next-Offset")
						io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
						if resp.StatusCode != 200 {
							continue
						}
						n, _ := offsetSeq(next)
						for {
							cur := maxAcked.Load()
							if int64(n) <= cur || maxAcked.CompareAndSwap(cur, int64(n)) {
								break
							}
						}
						// Some offset `expected` is known acked BEFORE this read
						// starts. A read now (strictly after) must observe >= it —
						// including writes acked to the other producers.
						expected := maxAcked.Load()
						rr, _ := http.NewRequest("GET", ts.URL+"/probe?offset=-1", nil)
						rresp, err := cl.Do(rr)
						if err != nil {
							t.Errorf("read: %v", err)
							return
						}
						rnext := rresp.Header.Get("Stream-Next-Offset")
						io.Copy(io.Discard, rresp.Body)
						rresp.Body.Close()
						readTail, _ := offsetSeq(rnext)
						if int64(readTail) < expected {
							if violations.Add(1) <= 3 {
								t.Logf("read-after-ack lag: offset %d acked before read, read saw tail %d", expected, readTail)
							}
						}
					}
				}(p)
			}
			wg.Wait()

			v := violations.Load()
			t.Logf("mode=%s: %d read-after-ack violations", mode, v)
			if v != 0 {
				t.Errorf("%s mode must guarantee read-after-ack, got %d violations", mode, v)
			}
		})
	}
}

// TestProbeReadAfterCloseStrict is a direct, causally-ordered probe (like
// TestProbeReadAfterAck) for the append-vs-close interaction
// porcupine_lifecycle_test.go's TestLinearizableStreamLifecycle can't
// soundly check via real-time intervals in notifier mode (see that file's
// package comment): after a close's own response is received, a
// strictly-subsequent read must reflect at least everything that was
// acknowledged before the close returned. Unlike the Porcupine check, this
// establishes happens-before by blocking on each response rather than by
// comparing concurrent operations' Call/Return intervals, so it stays sound
// regardless of the writer-view/durable-view skew notifier mode has for
// append-vs-close ordering specifically.
func TestProbeReadAfterCloseStrict(t *testing.T) {
	if testing.Short() {
		t.Skip("read-after-close probe skipped in -short")
	}
	const iterations = 100
	for _, mode := range []string{"sync", "notifier"} {
		t.Run(mode, func(t *testing.T) {
			violations := 0
			for iter := 0; iter < iterations; iter++ {
				ts, _ := startServer(t, mustMemStore(t), Config{
					Durability:           mode,
					NotifierPollInterval: time.Millisecond,
					LongPollTimeout:      100 * time.Millisecond,
					MaxReadBytes:         1 << 20,
				})
				resp := do(t, ts, "PUT", "/closeprobe", "", hdr("Content-Type", "application/json"))
				wantStatus(t, resp, 201)
				resp.Body.Close()

				cl := &http.Client{Timeout: 10 * time.Second}
				var maxAcked atomic.Int64
				var wg sync.WaitGroup

				// Fire N concurrent appends. Each goroutine tracks maxAcked only
				// after ITS OWN response returns (real happens-before for that
				// append, same discipline as TestProbeReadAfterAck).
				const n = 8
				for i := 0; i < n; i++ {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						body := fmt.Sprintf(`["v%d"]`, i)
						r, _ := http.NewRequest("POST", ts.URL+"/closeprobe", strings.NewReader(body))
						r.Header.Set("Content-Type", "application/json")
						resp, err := cl.Do(r)
						if err != nil {
							t.Errorf("append: %v", err)
							return
						}
						next := resp.Header.Get("Stream-Next-Offset")
						io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
						if seq, ok := offsetSeq(next); ok {
							for {
								cur := maxAcked.Load()
								if int64(seq) <= cur || maxAcked.CompareAndSwap(cur, int64(seq)) {
									break
								}
							}
						}
					}(i)
				}
				wg.Wait()

				// Close, and wait for its own response — establishing real
				// happens-before for everything acked so far.
				r, _ := http.NewRequest("POST", ts.URL+"/closeprobe", nil)
				r.Header.Set("Stream-Closed", "true")
				resp, err := cl.Do(r)
				if err != nil {
					t.Fatalf("close: %v", err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()

				expected := maxAcked.Load()

				// Strictly after the close's own response: a read must see at
				// least `expected` records.
				rr, _ := http.NewRequest("GET", ts.URL+"/closeprobe?offset=-1", nil)
				rresp, err := cl.Do(rr)
				if err != nil {
					t.Fatalf("read: %v", err)
				}
				b, _ := io.ReadAll(rresp.Body)
				rresp.Body.Close()
				values, _ := decodeJSONArray(b)
				got := int64(len(values))
				if got < expected {
					violations++
					if violations <= 5 {
						t.Logf("iter=%d: %d acked before close returned, read after close saw only %d", iter, expected, got)
					}
				}
			}
			if violations > 0 {
				t.Errorf("%s mode: %d/%d read-after-close-ack violations", mode, violations, iterations)
			}
		})
	}
}

func mustMemStore(t *testing.T) Storage {
	t.Helper()
	store, err := OpenStore(uniqueDBPath("probe"), "memory:///", StoreTuning{FlushInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// Note: startServer's cleanup closes the store via srv.Close(); don't double-close.
	return store
}
