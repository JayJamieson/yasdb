package ds

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestContentTypeMismatch(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, "PUT", "/ct", "", hdr("Content-Type", "text/plain")).Body.Close()
	resp := do(t, ts, "POST", "/ct", "x", hdr("Content-Type", "application/json"))
	wantStatus(t, resp, 409)
	resp.Body.Close()
}

func TestBadRequests(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, "PUT", "/b", "", hdr("Content-Type", "text/plain")).Body.Close()

	// malformed offset
	resp := do(t, ts, "GET", "/b?offset=not-an-offset", "")
	wantStatus(t, resp, 400)
	resp.Body.Close()

	// TTL + Expires-At conflict
	resp = do(t, ts, "PUT", "/b2", "", hdr("Content-Type", "text/plain"), hdr("Stream-TTL", "60"), hdr("Stream-Expires-At", "2030-01-01T00:00:00Z"))
	wantStatus(t, resp, 400)
	resp.Body.Close()

	// non-canonical TTL
	resp = do(t, ts, "PUT", "/b3", "", hdr("Content-Type", "text/plain"), hdr("Stream-TTL", "060"))
	wantStatus(t, resp, 400)
	resp.Body.Close()

	// method not allowed
	resp = do(t, ts, "PATCH", "/b", "")
	wantStatus(t, resp, 405)
	resp.Body.Close()

	// reserved __ds prefix
	resp = do(t, ts, "GET", "/__ds/subscriptions/x", "")
	wantStatus(t, resp, 501)
	resp.Body.Close()
}

func TestTTLReportedInHead(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, "PUT", "/ttl", "", hdr("Content-Type", "text/plain"), hdr("Stream-TTL", "3600")).Body.Close()
	resp := do(t, ts, "HEAD", "/ttl", "")
	wantStatus(t, resp, 200)
	if resp.Header.Get("Stream-TTL") != "3600" {
		t.Fatalf("HEAD Stream-TTL = %q", resp.Header.Get("Stream-TTL"))
	}
	resp.Body.Close()
}

func TestDormancyRespawn(t *testing.T) {
	srv, err := NewServer(openTestStore(t, 5*time.Millisecond), Config{DormancyTimeout: 40 * time.Millisecond, LongPollTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer func() { ts.Close(); _ = srv.Close() }()

	do(t, ts, "PUT", "/dr", "", hdr("Content-Type", "text/plain")).Body.Close()
	do(t, ts, "POST", "/dr", "one", hdr("Content-Type", "text/plain")).Body.Close()

	// let the streamer go dormant
	time.Sleep(120 * time.Millisecond)

	// appending re-spawns from durable tail and continues the sequence
	resp := do(t, ts, "POST", "/dr", "two", hdr("Content-Type", "text/plain"))
	wantStatus(t, resp, 204)
	if off := resp.Header.Get("Stream-Next-Offset"); off != "0000000000000002_0000000000000000" {
		t.Fatalf("post-dormancy offset = %q, want ...0002...", off)
	}
	resp.Body.Close()

	resp = do(t, ts, "GET", "/dr?offset=-1", "")
	if body := readBodyStr(t, resp); body != "onetwo" {
		t.Fatalf("post-dormancy read = %q", body)
	}
}

func TestConcurrentAppends(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, "PUT", "/cc", "", hdr("Content-Type", "application/json")).Body.Close()

	const goroutines = 8
	const perG = 25
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				body := fmt.Sprintf(`{"g":%d,"i":%d}`, g, i)
				resp := do(t, ts, "POST", "/cc", body, hdr("Content-Type", "application/json"))
				if resp.StatusCode != 204 {
					t.Errorf("append status %d", resp.StatusCode)
				}
				resp.Body.Close()
			}
		}(g)
	}
	wg.Wait()

	resp := do(t, ts, "HEAD", "/cc", "")
	wantOffset := fmt.Sprintf("%016d_0000000000000000", goroutines*perG)
	if got := resp.Header.Get("Stream-Next-Offset"); got != wantOffset {
		t.Fatalf("tail after concurrent appends = %q, want %q", got, wantOffset)
	}
	resp.Body.Close()

	// every (g,i) message must be present exactly once
	resp = do(t, ts, "GET", "/cc?offset=-1", "")
	body := readBodyStr(t, resp)
	var msgs []struct{ G, I int }
	if err := json.Unmarshal([]byte(body), &msgs); err != nil {
		t.Fatalf("unmarshal read: %v", err)
	}
	if len(msgs) != goroutines*perG {
		t.Fatalf("read %d messages, want %d", len(msgs), goroutines*perG)
	}
	seen := make(map[[2]int]bool)
	for _, m := range msgs {
		k := [2]int{m.G, m.I}
		if seen[k] {
			t.Fatalf("duplicate message %v", k)
		}
		seen[k] = true
	}
	if len(seen) != goroutines*perG {
		t.Fatalf("unique messages %d, want %d", len(seen), goroutines*perG)
	}
}

func TestNowOnClosedStream(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, "PUT", "/nc", "", hdr("Content-Type", "text/plain")).Body.Close()
	do(t, ts, "POST", "/nc", "data", hdr("Content-Type", "text/plain"), hdr("Stream-Closed", "true")).Body.Close()

	// catch-up offset=now on closed -> 200 empty + closed + up-to-date
	resp := do(t, ts, "GET", "/nc?offset=now", "")
	wantStatus(t, resp, 200)
	if resp.Header.Get("Stream-Closed") != "true" || resp.Header.Get("Stream-Up-To-Date") != "true" {
		t.Fatalf("now-on-closed headers: closed=%q utd=%q", resp.Header.Get("Stream-Closed"), resp.Header.Get("Stream-Up-To-Date"))
	}
	readBodyStr(t, resp)

	// long-poll offset=now on closed -> immediate 204 + closed
	start := time.Now()
	resp = do(t, ts, "GET", "/nc?offset=now&live=long-poll", "")
	wantStatus(t, resp, 204)
	if resp.Header.Get("Stream-Closed") != "true" {
		t.Fatalf("now long-poll on closed should report closed")
	}
	if time.Since(start) > 150*time.Millisecond {
		t.Fatalf("now long-poll on closed should be immediate")
	}
	resp.Body.Close()
}

// TestLongPollDeleteNoGhost is the regression for the zombie-streamer
// race: once DELETE has returned, a long-poll must behave exactly like
// catch-up — 404 with no record data — even though background record
// deletion may still be running. (Previously, a stale streamer could be
// spawned from already-deleted meta and keep serving ghost records to
// long-poll/SSE while catch-up 404'd.)
func TestLongPollDeleteNoGhost(t *testing.T) {
	ts := newTestServer(t)
	for i := 0; i < 60; i++ {
		path := fmt.Sprintf("/ghost/%d", i)
		do(t, ts, "PUT", path, "", hdr("Content-Type", "text/plain")).Body.Close()
		do(t, ts, "POST", path, "ghost", hdr("Content-Type", "text/plain")).Body.Close()

		// DELETE returns only after the meta-clear is durable, so every read
		// issued afterwards is a "subsequent" read and must not see data.
		do(t, ts, "DELETE", path, "").Body.Close()

		lp := do(t, ts, "GET", path+"?offset=0000000000000000_0000000000000000&live=long-poll", "")
		lpBody := readBodyStr(t, lp)
		cat := do(t, ts, "GET", path+"?offset=-1", "")
		cat.Body.Close()

		if strings.Contains(lpBody, "ghost") {
			t.Fatalf("iter %d: long-poll served ghost data after delete: status %d body %q", i, lp.StatusCode, lpBody)
		}
		if lp.StatusCode != cat.StatusCode {
			t.Fatalf("iter %d: long-poll %d != catch-up %d after delete", i, lp.StatusCode, cat.StatusCode)
		}
		if lp.StatusCode != 404 {
			t.Fatalf("iter %d: read after delete = %d, want 404 (body %q)", i, lp.StatusCode, lpBody)
		}
	}
}

// TestInFlightLongPollTerminatesOnDelete verifies a parked long-poll returns
// promptly (404) when its stream is deleted, instead of hanging to timeout.
func TestInFlightLongPollTerminatesOnDelete(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, "PUT", "/inflight", "", hdr("Content-Type", "text/plain")).Body.Close()

	done := make(chan int, 1)
	go func() {
		// park at the tail (no data) so the long-poll waits
		resp := do(t, ts, "GET", "/inflight?offset=now&live=long-poll", "")
		done <- resp.StatusCode
		resp.Body.Close()
	}()

	time.Sleep(60 * time.Millisecond) // let it park
	start := time.Now()
	do(t, ts, "DELETE", "/inflight", "").Body.Close()

	select {
	case code := <-done:
		if time.Since(start) > 200*time.Millisecond {
			t.Fatalf("parked long-poll took %v to notice delete", time.Since(start))
		}
		if code != 404 {
			t.Fatalf("parked long-poll after delete returned %d, want 404", code)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("parked long-poll did not terminate after delete")
	}
}

func newTestServerCfg(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	if cfg.Durability == "" {
		cfg.Durability = testDurability()
	}
	srv, err := NewServer(openTestStore(t, 5*time.Millisecond), cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { ts.Close(); _ = srv.Close() })
	return ts
}

func waitExpired(t *testing.T, ts *httptest.Server, path string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		resp := do(t, ts, "HEAD", path, "")
		code := resp.StatusCode
		resp.Body.Close()
		if code == 404 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("stream %s did not expire within %v", path, within)
}

func TestTTLExpiry(t *testing.T) {
	ts := newTestServerCfg(t, Config{SweepInterval: 150 * time.Millisecond})

	do(t, ts, "PUT", "/ttl/idle", "", hdr("Content-Type", "text/plain"), hdr("Stream-TTL", "1")).Body.Close()
	// alive right after create
	resp := do(t, ts, "HEAD", "/ttl/idle", "")
	wantStatus(t, resp, 200)
	resp.Body.Close()
	// expires while idle
	waitExpired(t, ts, "/ttl/idle", 4*time.Second)
	// GET and POST also 404 after expiry
	resp = do(t, ts, "GET", "/ttl/idle?offset=-1", "")
	wantStatus(t, resp, 404)
	resp.Body.Close()
	// recreatable after expiry (no blocking tombstone)
	resp = do(t, ts, "PUT", "/ttl/idle", "", hdr("Content-Type", "text/plain"))
	wantStatus(t, resp, 201)
	resp.Body.Close()
}

func TestExpiresAtExpiry(t *testing.T) {
	ts := newTestServerCfg(t, Config{SweepInterval: 150 * time.Millisecond})
	expiresAt := time.Now().Add(1 * time.Second).UTC().Format(time.RFC3339)
	do(t, ts, "PUT", "/exp/s", "", hdr("Content-Type", "text/plain"), hdr("Stream-Expires-At", expiresAt)).Body.Close()
	resp := do(t, ts, "HEAD", "/exp/s", "")
	wantStatus(t, resp, 200)
	if resp.Header.Get("Stream-Expires-At") == "" {
		t.Fatalf("HEAD should report Stream-Expires-At")
	}
	resp.Body.Close()
	waitExpired(t, ts, "/exp/s", 5*time.Second)
}

func TestTTLWriteKeepsAlive(t *testing.T) {
	ts := newTestServerCfg(t, Config{SweepInterval: 150 * time.Millisecond})
	do(t, ts, "PUT", "/ttl/active", "", hdr("Content-Type", "text/plain"), hdr("Stream-TTL", "2")).Body.Close()
	// append past the original deadline; the write must slide the TTL forward
	for i := 0; i < 6; i++ {
		time.Sleep(400 * time.Millisecond)
		resp := do(t, ts, "POST", "/ttl/active", "x", hdr("Content-Type", "text/plain"))
		if resp.StatusCode != 204 {
			t.Fatalf("append %d after %v: status %d (stream expired despite writes?)", i, time.Duration(i)*400*time.Millisecond, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestPartialRecordDelivery(t *testing.T) {
	// Small read cap so a single record must be split across reads.
	ts := newTestServerCfg(t, Config{MaxReadBytes: 100})
	do(t, ts, "PUT", "/big", "", hdr("Content-Type", "application/octet-stream")).Body.Close()

	// One 250-byte record.
	payload := strings.Repeat("A", 100) + strings.Repeat("B", 100) + strings.Repeat("C", 50)
	do(t, ts, "POST", "/big", payload, hdr("Content-Type", "application/octet-stream")).Body.Close()

	var got strings.Builder
	offset := "-1"
	reads := 0
	for {
		resp := do(t, ts, "GET", "/big?offset="+offset, "")
		wantStatus(t, resp, 200)
		chunk := readBodyStr(t, resp)
		got.WriteString(chunk)
		next := resp.Header.Get("Stream-Next-Offset")
		reads++
		if resp.Header.Get("Stream-Up-To-Date") == "true" {
			// verify a mid-record offset appeared (byte component non-zero)
			break
		}
		if len(chunk) > 100 {
			t.Fatalf("chunk exceeded cap: %d bytes", len(chunk))
		}
		offset = next
		if reads > 10 {
			t.Fatalf("too many reads; offset not advancing (last next=%q)", next)
		}
	}
	if got.String() != payload {
		t.Fatalf("reassembled %q, want %q", got.String(), payload)
	}
	if reads < 3 {
		t.Fatalf("expected the 250-byte record to span >=3 reads of 100B, got %d", reads)
	}

	// A resumed offset with a non-zero byte component must be usable directly.
	resp := do(t, ts, "GET", "/big?offset=0000000000000000_0000000000000100", "")
	wantStatus(t, resp, 200)
	mid := readBodyStr(t, resp)
	if !strings.HasPrefix(mid, "B") {
		t.Fatalf("resume at byte 100 should start with B's, got %q...", mid[:min(10, len(mid))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestConcurrentCloseNoDeadlock exercises Server.Close() while streamers are
// retiring on dormancy — the two paths take the registry shard lock and st.mu
// in opposite orders, so a naive Close deadlocks. A hang here is caught by
// the timeout.
func TestConcurrentCloseNoDeadlock(t *testing.T) {
	for trial := 0; trial < 10; trial++ {
		srv, err := NewServer(openTestStore(t, 5*time.Millisecond), Config{DormancyTimeout: time.Millisecond, SweepInterval: 5 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(srv)

		// Spawn many streamers; with a 1ms dormancy they begin retiring at once.
		for i := 0; i < 40; i++ {
			path := fmt.Sprintf("/cs%d", i)
			do(t, ts, "PUT", path, "", hdr("Content-Type", "text/plain")).Body.Close()
			do(t, ts, "POST", path, "x", hdr("Content-Type", "text/plain")).Body.Close()
		}

		done := make(chan struct{})
		go func() { _ = srv.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("trial %d: Server.Close deadlocked", trial)
		}
		ts.Close()
	}
}

var _ = http.MethodGet
