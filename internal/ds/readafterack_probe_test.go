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

func mustMemStore(t *testing.T) Storage {
	t.Helper()
	store, err := OpenStore(uniqueDBPath("probe"), "memory:///", StoreTuning{FlushInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// Note: startServer's cleanup closes the store via srv.Close(); don't double-close.
	return store
}
