package ds

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newLiveTestServer builds a server over the default test store (fake unless
// YASDB_TEST_BACKEND=slatedb) and returns both the endpoint and the underlying
// *Server so tests can inspect streamer state.
func newLiveTestServer(tb testing.TB, cfg Config, flush time.Duration) (*httptest.Server, *Server) {
	tb.Helper()
	return startServer(tb, openTestStore(tb, flush), cfg)
}

// TestSSEControlFrameMatchesJSON pins the hand-rolled control frame
// (appendSSEControl) to exactly what json.Marshal of the equivalent tagged struct
// would produce, across offsets / cursors / flag combinations.
func TestSSEControlFrameMatchesJSON(t *testing.T) {
	type sseControlJSON struct {
		StreamNextOffset string `json:"streamNextOffset"`
		StreamCursor     string `json:"streamCursor,omitempty"`
		UpToDate         bool   `json:"upToDate,omitempty"`
		StreamClosed     bool   `json:"streamClosed,omitempty"`
	}
	offs := []Offset{{0, 0}, {42, 0}, {1234567890123, 456}, {999999999999999, 12}}
	cursors := []string{"", "0", "12345", "18446744073709"}
	for _, o := range offs {
		for _, cur := range cursors {
			for _, up := range []bool{false, true} {
				for _, cl := range []bool{false, true} {
					b, _ := json.Marshal(sseControlJSON{o.String(), cur, up, cl})
					want := "event: control\ndata:" + string(b) + "\n\n"
					got := string(appendSSEControl(nil, sseControl{Next: o, StreamCursor: cur, UpToDate: up, StreamClosed: cl}))
					if got != want {
						t.Fatalf("mismatch off=%v cur=%q up=%v cl=%v\n got:  %q\n want: %q", o, cur, up, cl, got, want)
					}
				}
			}
		}
	}
}

// TestLiveCacheMatchesStore is the differential correctness guard for the record
// cache: whenever the cache serves a read (ok=true) it must return exactly what a
// store scan (readRange) would — same body bytes, next offset, and up-to-date
// flag — for both binary and JSON streams.
func TestLiveCacheMatchesStore(t *testing.T) {
	cases := []struct {
		name string
		ct   string
		rec  func(i int) []byte
	}{
		{"binary", "text/plain", func(i int) []byte {
			return []byte(fmt.Sprintf("r%d-%s", i, strings.Repeat("x", i%5)))
		}},
		{"json", "application/json", func(i int) []byte {
			return []byte(fmt.Sprintf("{\"i\":%d}", i))
		}},
	}
	const n = 200
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, srv := newLiveTestServer(t, Config{}, 5*time.Millisecond)
			do(t, ts, "PUT", "/diff", "", hdr("Content-Type", tc.ct)).Body.Close()

			st, found, err := srv.getOrSpawn("/diff")
			if err != nil || !found {
				t.Fatalf("spawn: found=%v err=%v", found, err)
			}
			st.readers.Add(1) // so applyReader populates the cache

			for i := 0; i < n; i++ {
				resp, ok := srv.submitAppend("/diff", appendReq{
					records: [][]byte{tc.rec(i)}, hasBody: true, contentType: tc.ct,
				})
				if !ok || (resp.status != 204 && resp.status != 200) {
					t.Fatalf("append %d: ok=%v status=%d", i, ok, resp.status)
				}
			}
			tail := st.tail.Load()
			if tail != n {
				t.Fatalf("tail=%d want %d", tail, n)
			}

			served := 0
			for start := uint64(0); start < tail; start++ {
				so := Offset{Seq: start}
				cb, cn, cu, ok := st.cache.tryRead(nil, st.isJSON, so, tail, srv.cfg.MaxReadBytes)
				if !ok {
					continue
				}
				served++
				rb, rn, ru, err := srv.readRange(st.id, st.forkedFrom, st.forkOffset, st.isJSON, so, tail, srv.cfg.MaxReadBytes)
				if err != nil {
					t.Fatalf("readRange(start=%d): %v", start, err)
				}
				if !bytes.Equal(cb, rb) {
					t.Fatalf("start=%d: cache body %q != store body %q", start, cb, rb)
				}
				if cn != rn || cu != ru {
					t.Fatalf("start=%d: cache (next=%v up=%v) != store (next=%v up=%v)", start, cn, cu, rn, ru)
				}
			}
			if served == 0 {
				t.Fatal("cache served no reads; differential check was vacuous")
			}
			// A mid-record byte offset must never be served from cache.
			if _, _, _, ok := st.cache.tryRead(nil, st.isJSON, Offset{Seq: 0, Byte: 1}, tail, srv.cfg.MaxReadBytes); ok {
				t.Fatal("cache served a non-zero byte offset; should fall back")
			}
			t.Logf("%s: cache matched store on %d/%d start offsets", tc.name, served, tail)
		})
	}
}

// FuzzLiveCacheMatchesStore is the property version of TestLiveCacheMatchesStore:
// the fuzzer picks the content type, per-read cap, and record sizes (including
// records larger than the cap, which force the partial-record / cap fallback),
// then asserts that wherever the cache serves a read it is byte-for-byte identical
// to a store scan (readRange). The two code paths are oracles for each other.
func FuzzLiveCacheMatchesStore(f *testing.F) {
	f.Add([]byte{0x00, 3, 1, 2, 3, 2, 9, 9, 0})       // binary, small records
	f.Add([]byte{0x01, 5, 1, 2, 3, 4, 5, 0, 2, 7, 7}) // json-typed, includes an empty record
	f.Add([]byte{0x02, 40, 1, 2, 3})                  // tiny cap + an over-cap record (fallback)

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 2 {
			return
		}
		isJSON := data[0]&1 == 1
		limit := []uint64{16, 64, 1 << 20}[(data[0]>>1)%3]
		ct := "text/plain"
		if isJSON {
			ct = "application/json"
		}
		// Derive records from the remaining bytes: a length byte then that many
		// bytes, repeated.
		var recs [][]byte
		for i := 1; i < len(data) && len(recs) < 64; {
			n := int(data[i])
			i++
			if n > len(data)-i {
				n = len(data) - i
			}
			recs = append(recs, append([]byte(nil), data[i:i+n]...))
			i += n
		}
		if len(recs) == 0 {
			return
		}

		ts, srv := newLiveTestServer(t, Config{MaxReadBytes: limit}, time.Millisecond)
		do := func(method, path string) {
			req, _ := http.NewRequest(method, ts.URL+path, nil)
			req.Header.Set("Content-Type", ct)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
		}
		do("PUT", "/fz")
		st, found, err := srv.getOrSpawn("/fz")
		if err != nil || !found {
			t.Fatalf("spawn: %v", err)
		}
		st.readers.Add(1) // so the commit populates the cache
		if resp, ok := srv.submitAppend("/fz", appendReq{records: recs, hasBody: true, contentType: ct}); !ok || (resp.status != 204 && resp.status != 200) {
			t.Fatalf("append: ok=%v status=%d", ok, resp.status)
		}
		tail := st.tail.Load()

		for start := uint64(0); start < tail; start++ {
			so := Offset{Seq: start}
			cb, cn, cu, ok := st.cache.tryRead(nil, st.isJSON, so, tail, limit)
			if !ok {
				continue
			}
			rb, rn, ru, rerr := srv.readRange(st.id, st.forkedFrom, st.forkOffset, st.isJSON, so, tail, limit)
			if rerr != nil {
				t.Fatalf("readRange(start=%d): %v", start, rerr)
			}
			if !bytes.Equal(cb, rb) || cn != rn || cu != ru {
				t.Fatalf("mismatch start=%d isJSON=%v limit=%d\n cache: body=%q next=%v up=%v\n store: body=%q next=%v up=%v",
					start, st.isJSON, limit, cb, cn, cu, rb, rn, ru)
			}
		}
		// A mid-record byte offset must never be served from the cache.
		if _, _, _, ok := st.cache.tryRead(nil, st.isJSON, Offset{Seq: 0, Byte: 1}, tail, limit); ok {
			t.Fatal("cache served a non-zero byte offset; should fall back")
		}
	})
}

type fanoutResult struct {
	readers     int
	commits     int
	wakes       int64
	appendWall  time.Duration
	drainWall   time.Duration
	totalWall   time.Duration
	deliverPerS float64
	err         error
}

// runFanout connects `readers` live SSE readers at offset=-1, then a single
// writer lands `commits` sequential appends. Each reader byte-verifies it
// receives records 0..commits-1 in order. Returns timing plus the streamer's
// broadcast count.
func runFanout(tb testing.TB, ts *httptest.Server, srv *Server, path string, readers, commits, recW int) fanoutResult {
	tb.Helper()
	res := fanoutResult{readers: readers, commits: commits}

	putReq, _ := http.NewRequest("PUT", ts.URL+path, nil)
	putReq.Header.Set("Content-Type", "text/plain")
	if resp, err := http.DefaultClient.Do(putReq); err != nil {
		res.err = fmt.Errorf("create: %w", err)
		return res
	} else {
		resp.Body.Close()
	}

	rec := func(i int) string { return fmt.Sprintf("%0*d", recW, i) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sseURL := ts.URL + path + "?offset=-1&live=sse"

	var (
		ready    sync.WaitGroup // readers signal once connected (HTTP 200)
		done     sync.WaitGroup // readers signal once they have all records
		firstErr atomic.Value   // error
		client   = &http.Client{}
	)
	ready.Add(readers)
	done.Add(readers)

	for r := 0; r < readers; r++ {
		go func(id int) {
			defer done.Done()
			req, _ := http.NewRequestWithContext(ctx, "GET", sseURL, nil)
			resp, err := client.Do(req)
			if err != nil {
				if firstErr.Load() == nil {
					firstErr.Store(fmt.Errorf("reader %d connect: %w", id, err))
				}
				ready.Done()
				return
			}
			defer resp.Body.Close()
			ready.Done()

			sc := bufio.NewScanner(resp.Body)
			sc.Buffer(make([]byte, 1<<16), 1<<20)
			isData := false
			next := 0 // next record index expected
			for sc.Scan() {
				line := sc.Text()
				switch {
				case line == "event: data":
					isData = true
				case strings.HasPrefix(line, "event:"):
					isData = false
				case strings.HasPrefix(line, "data:") && isData:
					payload := strings.TrimPrefix(line, "data:")
					for off := 0; off+recW <= len(payload); off += recW {
						got := payload[off : off+recW]
						if got != rec(next) {
							if firstErr.Load() == nil {
								firstErr.Store(fmt.Errorf("reader %d: at %d got %q want %q", id, next, got, rec(next)))
							}
							return
						}
						next++
						if next == commits {
							return
						}
					}
				}
			}
			if next != commits && firstErr.Load() == nil {
				firstErr.Store(fmt.Errorf("reader %d: stream ended at %d/%d", id, next, commits))
			}
		}(r)
	}

	ready.Wait() // all readers parked before the first commit

	st, _, err := srv.getOrSpawn(path)
	if err != nil {
		res.err = fmt.Errorf("spawn: %w", err)
		return res
	}
	wake0 := st.wakeCount.Load()

	start := time.Now()
	for i := 0; i < commits; i++ {
		resp, ok := srv.submitAppend(path, appendReq{
			records: [][]byte{[]byte(rec(i))}, hasBody: true, contentType: "text/plain",
		})
		if !ok || (resp.status != 204 && resp.status != 200) {
			res.err = fmt.Errorf("append %d: ok=%v status=%d", i, ok, resp.status)
			return res
		}
	}
	res.appendWall = time.Since(start)

	drainStart := time.Now()
	waitCh := make(chan struct{})
	go func() { done.Wait(); close(waitCh) }()
	select {
	case <-waitCh:
	case <-time.After(30 * time.Second):
		res.err = fmt.Errorf("timeout waiting for readers to drain")
	}
	cancel()
	res.drainWall = time.Since(drainStart)
	res.totalWall = time.Since(start)
	res.wakes = st.wakeCount.Load() - wake0
	res.deliverPerS = float64(readers*commits) / res.totalWall.Seconds()
	if e := firstErr.Load(); e != nil {
		res.err = e.(error)
	}
	return res
}

// TestLiveFanoutCompare runs the same high-commit fan-out under v1 (wake on every
// commit) and v2 (coalesced wakeups) and asserts correctness, a deterministic
// wake-rate bound, fewer broadcasts, and no end-to-end regression.
func TestLiveFanoutCompare(t *testing.T) {
	if testing.Short() {
		t.Skip("skip fan-out comparison in -short")
	}
	const (
		readers = 100
		commits = 300
		recW    = 8
		flush   = time.Millisecond
		// Window sized well above the worst-case per-commit interval (even under
		// -race) so folding is guaranteed.
		window = 100 * time.Millisecond
	)

	runOne := func(w time.Duration) fanoutResult {
		ts, srv := newRealLiveServer(t, Config{LiveCoalesceWindow: w}, flush)
		return runFanout(t, ts, srv, "/fan", readers, commits, recW)
	}

	v1 := runOne(0)
	v2 := runOne(window)

	for name, r := range map[string]fanoutResult{"v1": v1, "v2": v2} {
		if r.err != nil {
			t.Fatalf("%s: correctness failure: %v", name, r.err)
		}
	}

	t.Logf("%-3s  readers=%d commits=%d  wakes=%-6d append=%-8s drain=%-8s total=%-8s deliver=%.0f rec/s",
		"v1", v1.readers, v1.commits, v1.wakes, v1.appendWall.Round(time.Millisecond),
		v1.drainWall.Round(time.Millisecond), v1.totalWall.Round(time.Millisecond), v1.deliverPerS)
	t.Logf("%-3s  readers=%d commits=%d  wakes=%-6d append=%-8s drain=%-8s total=%-8s deliver=%.0f rec/s",
		"v2", v2.readers, v2.commits, v2.wakes, v2.appendWall.Round(time.Millisecond),
		v2.drainWall.Round(time.Millisecond), v2.totalWall.Round(time.Millisecond), v2.deliverPerS)

	// (1) Baseline is the thundering herd: v1 broadcasts ~once per commit.
	if v1.wakes < int64(commits)*9/10 {
		t.Errorf("expected v1 to wake ~once per commit, got %d wakes for %d commits", v1.wakes, commits)
	}
	// (2) Deterministic rate bound: leading-edge debounce keeps consecutive wakes
	// >= window apart, so v2 fires at most ~totalWall/window times. Holds even
	// under -race (timers fire late, never early).
	bound := int64(v2.totalWall/window) + 5
	if v2.wakes > bound {
		t.Errorf("v2 wakeups exceed the coalesce bound: wakes=%d > totalWall/window+5=%d", v2.wakes, bound)
	}
	// (3) Real reduction: v2 folds many commits per wake.
	if v2.wakes*2 > v1.wakes {
		t.Errorf("expected v2 to coalesce wakeups: v1=%d v2=%d (want v2 <= v1/2)", v1.wakes, v2.wakes)
	}
	// (4) No gross end-to-end regression from coalescing.
	if v2.totalWall > v1.totalWall*3/2+window {
		t.Errorf("v2 delivery regressed: v1=%s v2=%s", v1.totalWall, v2.totalWall)
	}
}
