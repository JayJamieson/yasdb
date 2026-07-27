package ds

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Read-heavy stress: one writer appends a dense, self-describing log (record i
// has body "%08d" of i) while many concurrent readers hammer every read path —
// catch-up, long-poll, and SSE. Because each record's content encodes its own
// position, any reordering, gap, duplication, phantom, or torn record is caught
// by a byte-level content check, and the tail can only ever grow. This stresses
// the read side (broadcast fan-out to dozens of waiters, per-record iterator
// FFI, offset assembly) that the write-side benchmarks don't.
//
// Runs in both durability modes (YASDB_TEST_DURABILITY). Skipped under -short.
func TestReadHeavyStress(t *testing.T) {
	if testing.Short() {
		t.Skip("read-heavy stress skipped in -short")
	}

	const (
		recW        = 8 // width of "%08d"
		catchupRead = 40
		longpollN   = 12
		sseN        = 8
		appendFor   = 4 * time.Second
		maxRecords  = 8000 // keep total well under MaxReadBytes so no partial records
	)

	ts := newTestServerCfg(t, Config{
		LongPollTimeout:      250 * time.Millisecond,
		SSELifetime:          time.Second, // short: SSE readers reconnect and notice "caught up" promptly
		NotifierPollInterval: time.Millisecond,
		LiveCoalesceWindow:   5 * time.Millisecond, // exercise the coalesced wake path under -race
	})

	// create a text/plain stream
	resp := do(t, ts, "PUT", "/stress", "", hdr("Content-Type", "text/plain"))
	wantStatus(t, resp, 201)
	resp.Body.Close()

	rec := func(i int) string { return fmt.Sprintf("%0*d", recW, i) }

	// verifyPrefix asserts body is exactly records [base, base+len/recW) in order.
	verifyPrefix := func(t *testing.T, who string, base int, body string) int {
		t.Helper()
		if len(body)%recW != 0 {
			t.Errorf("%s: body len %d not a multiple of %d (torn record)", who, len(body), recW)
			return base
		}
		n := len(body) / recW
		for j := 0; j < n; j++ {
			got := body[j*recW : j*recW+recW]
			want := rec(base + j)
			if got != want {
				t.Errorf("%s: record %d = %q, want %q (reorder/gap/dup)", who, base+j, got, want)
				return base + j
			}
		}
		return base + n
	}

	var (
		acked   atomic.Int64 // records durably appended so far
		done    atomic.Bool  // writer finished
		maxTail atomic.Int64 // max record count any reader has observed
		wg      sync.WaitGroup
	)
	observe := func(k int) {
		for {
			cur := maxTail.Load()
			if int64(k) <= cur || maxTail.CompareAndSwap(cur, int64(k)) {
				return
			}
		}
	}

	client := &http.Client{Timeout: 6 * time.Second}
	getBody := func(url string) (string, string, int) {
		req, _ := http.NewRequest("GET", ts.URL+url, nil)
		r, err := client.Do(req)
		if err != nil {
			return "", "", 0
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		return string(b), r.Header.Get("Stream-Next-Offset"), r.StatusCode
	}

	// ---- writer: one goroutine, sequential dense appends ----
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer done.Store(true)
		deadline := time.Now().Add(appendFor)
		for i := 0; time.Now().Before(deadline) && i < maxRecords; i++ {
			req, _ := http.NewRequest("POST", ts.URL+"/stress", strings.NewReader(rec(i)))
			req.Header.Set("Content-Type", "text/plain")
			r, err := client.Do(req)
			if err != nil {
				t.Errorf("writer: append %d: %v", i, err)
				return
			}
			b, _ := io.ReadAll(r.Body)
			r.Body.Close()
			if r.StatusCode != 204 { // plain append -> 204 No Content
				t.Errorf("writer: append %d -> %d (%s)", i, r.StatusCode, b)
				return
			}
			acked.Store(int64(i + 1))
		}
	}()

	caughtUp := func() bool { return done.Load() && maxTail.Load() >= acked.Load() }

	// ---- catch-up readers: re-read the whole stream, verify prefix ----
	for w := 0; w < catchupRead; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				body, _, code := getBody("/stress?offset=-1")
				if code == 200 {
					n := verifyPrefix(t, fmt.Sprintf("catchup%d", id), 0, body)
					observe(n)
				}
				if caughtUp() {
					return
				}
				time.Sleep(time.Millisecond)
			}
		}(w)
	}

	// ---- long-poll readers: follow the tail from a cursor ----
	for w := 0; w < longpollN; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			body, next, _ := getBody("/stress?offset=-1")
			expect := verifyPrefix(t, fmt.Sprintf("lp%d-init", id), 0, body)
			observe(expect)
			cursor := next
			for {
				b, nx, code := getBody("/stress?offset=" + cursor + "&live=long-poll")
				if code == 200 && len(b) > 0 {
					expect = verifyPrefix(t, fmt.Sprintf("lp%d", id), expect, b)
					observe(expect)
					if nx != "" {
						cursor = nx
					}
				}
				if caughtUp() {
					return
				}
			}
		}(w)
	}

	// ---- SSE readers: tail the live stream from the start ----
	for w := 0; w < sseN; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			expect := 0
			for { // reconnect until caught up (SSELifetime may lapse)
				expect = sseTail(t, client, ts.URL+"/stress?offset="+fmt.Sprintf("%016d_0000000000000000", expect)+"&live=sse",
					fmt.Sprintf("sse%d", id), expect, recW, verifyPrefix, observe, caughtUp)
				if caughtUp() {
					return
				}
			}
		}(w)
	}

	wg.Wait()

	// ---- final consistency: the whole log reads back exactly, in order ----
	total := int(acked.Load())
	if total < 50 {
		t.Fatalf("writer only appended %d records; too few to be a meaningful stress", total)
	}
	body, next, code := getBody("/stress?offset=-1")
	if code != 200 {
		t.Fatalf("final read -> %d", code)
	}
	if got := verifyPrefix(t, "final", 0, body); got != total {
		t.Fatalf("final read has %d records, want %d", got, total)
	}
	if wantSeq := fmt.Sprintf("%016d_0000000000000000", total); next != wantSeq {
		t.Fatalf("final Stream-Next-Offset = %q, want %q", next, wantSeq)
	}
	t.Logf("read-heavy stress ok: %d records, %d readers (%d catch-up / %d long-poll / %d SSE), max observed tail %d",
		total, catchupRead+longpollN+sseN, catchupRead, longpollN, sseN, maxTail.Load())
}

// sseTail consumes an SSE connection, verifying each data batch continues the
// dense log from `expect`. Returns the next expected index when the connection
// ends (timeout, close, or caught up).
func sseTail(
	t *testing.T,
	client *http.Client,
	url, who string,
	expect, recW int,
	verify func(*testing.T, string, int, string) int,
	observe func(int),
	caughtUp func() bool,
) int {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	resp, err := client.Do(req)
	if err != nil {
		return expect
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	var isData bool
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "event: data":
			isData = true
		case strings.HasPrefix(line, "event:"):
			isData = false
		case strings.HasPrefix(line, "data:") && isData:
			// text SSE: newline-free records concatenate into one data line
			expect = verify(t, who, expect, strings.TrimPrefix(line, "data:"))
			observe(expect)
		case line == "":
			if caughtUp() {
				return expect
			}
		}
	}
	return expect
}
