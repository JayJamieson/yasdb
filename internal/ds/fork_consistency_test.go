package ds

// Fork correctness: a fork's initial log must exactly equal a prefix of its
// source's log, truncated at the fork's own divergence offset. This is a
// cross-object invariant — the child depends on the parent's contemporaneous
// state at one specific instant — which doesn't fit Porcupine's per-object
// partitioned checking without real model surgery (Porcupine checks each
// partition independently; a fork needs a join across two partitions at one
// point in time). So it's verified here as a direct differential check
// instead, in the style of the existing TestLiveFanoutCompare.
//
// Since fork offset defaults to the source's tail at fork time, and streams
// are append-only under a single writer, the source's first N records (N =
// the fork's own divergence offset) are immutable by the time the fork
// responds — so a full catch-up read of the source afterward, truncated
// client-side to N records, is a stable comparison point even though the
// source keeps growing concurrently.

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestForkConsistencyUnderConcurrentWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("fork consistency check skipped in -short")
	}

	forks := envInt("YASDB_FORK_CHECKS", 20)

	ts, _ := newRealLiveServer(t, Config{
		LongPollTimeout: 200 * time.Millisecond,
		MaxReadBytes:    1 << 20,
	}, 5*time.Millisecond)

	srcPath := "/forksrc"
	resp := do(t, ts, "PUT", srcPath, "", hdr("Content-Type", "application/json"))
	wantStatus(t, resp, 201)
	resp.Body.Close()

	// Background writer: continuously appends to the source while forks are
	// created concurrently from the main goroutine below.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var appendErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		cl := &http.Client{Timeout: 15 * time.Second}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			body := fmt.Sprintf(`["v%d"]`, i)
			req, _ := http.NewRequest("POST", ts.URL+srcPath, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := cl.Do(req)
			if err != nil {
				mu.Lock()
				if appendErr == nil {
					appendErr = fmt.Errorf("background append %d: %w", i, err)
				}
				mu.Unlock()
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				mu.Lock()
				if appendErr == nil {
					appendErr = fmt.Errorf("background append %d: status %d", i, resp.StatusCode)
				}
				mu.Unlock()
				return
			}
		}
	}()

	for i := 0; i < forks; i++ {
		forkPath := fmt.Sprintf("/forkdst%d", i)
		resp := do(t, ts, "PUT", forkPath, "", hdr(hForkedFrom, srcPath))
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("fork %d: status %d body %q", i, resp.StatusCode, b)
		}
		divergenceTok := resp.Header.Get(hStreamNext)
		resp.Body.Close()
		divergenceSeq, ok := offsetSeq(divergenceTok)
		if !ok {
			t.Fatalf("fork %d: bad divergence offset %q", i, divergenceTok)
		}

		forkResp := do(t, ts, "GET", forkPath+"?offset=-1", "")
		wantStatus(t, forkResp, 200)
		forkRecords, err := decodeJSONArray([]byte(readBodyStr(t, forkResp)))
		if err != nil {
			t.Fatalf("fork %d: decode fork body: %v", i, err)
		}

		// createFork's default divergence offset comes from the source's live
		// gated tail (liveState), the same view a concurrent GET on the
		// source uses — so a read right after the fork must already be able
		// to prove at least this many records exist. A single read here is
		// the actual regression check for that guarantee: no retry/tolerance.
		srcResp := do(t, ts, "GET", srcPath+"?offset=-1", "")
		wantStatus(t, srcResp, 200)
		srcRecords, err := decodeJSONArray([]byte(readBodyStr(t, srcResp)))
		if err != nil {
			t.Fatalf("fork %d: decode source body: %v", i, err)
		}

		if len(srcRecords) < divergenceSeq {
			t.Fatalf("fork %d: source has only %d records, want at least %d (its own divergence offset)",
				i, len(srcRecords), divergenceSeq)
		}
		if !equalStrings(forkRecords, srcRecords[:divergenceSeq]) {
			t.Fatalf("fork %d: fork's initial log doesn't match the source's prefix at divergence offset %d\n  fork:       %v\n  src prefix: %v",
				i, divergenceSeq, forkRecords, srcRecords[:divergenceSeq])
		}
	}

	close(stop)
	wg.Wait()
	mu.Lock()
	err := appendErr
	mu.Unlock()
	if err != nil {
		t.Fatalf("background writer: %v", err)
	}
}
