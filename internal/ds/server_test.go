package ds

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var dbCounter atomic.Uint64

// testDurability lets the whole suite run in both durability modes:
// YASDB_TEST_DURABILITY=notifier go test ...
func testDurability() string { return os.Getenv("YASDB_TEST_DURABILITY") }

// uniqueDBPath returns a collision-free DB path/prefix for a test store.
func uniqueDBPath(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), dbCounter.Add(1))
}

// startServer wraps a store in a running httptest server, applying the suite's
// durability defaults and registering cleanup. It returns both the endpoint and
// the underlying *Server so tests can inspect streamer state.
func startServer(tb testing.TB, store Storage, cfg Config) (*httptest.Server, *Server) {
	tb.Helper()
	if cfg.Durability == "" {
		cfg.Durability = testDurability()
	}
	if cfg.NotifierPollInterval == 0 {
		cfg.NotifierPollInterval = time.Millisecond
	}
	srv, err := NewServer(store, cfg)
	if err != nil {
		tb.Fatalf("new server: %v", err)
	}
	ts := httptest.NewServer(srv)
	tb.Cleanup(func() { ts.Close(); srv.Close() })
	return ts, srv
}

// newRealLiveServer runs a server over the real SlateDB store regardless of
// YASDB_TEST_BACKEND. The fan-out timing checks and benchmarks depend on real
// flush cadence, which the fake (instant durability) does not model.
func newRealLiveServer(tb testing.TB, cfg Config, flush time.Duration) (*httptest.Server, *Server) {
	tb.Helper()
	store, err := OpenStore(uniqueDBPath("live"), "memory:///", flush)
	if err != nil {
		tb.Fatalf("open store: %v", err)
	}
	return startServer(tb, store, cfg)
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv, err := NewServer(openTestStore(t, 5*time.Millisecond), Config{
		LongPollTimeout: 300 * time.Millisecond,
		SSELifetime:     500 * time.Millisecond,
		Durability:      testDurability(),
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		ts.Close()
		srv.Close()
	})
	return ts
}

type reqOpt func(*http.Request)

func hdr(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

func do(t *testing.T, ts *httptest.Server, method, path, body string, opts ...reqOpt) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for _, o := range opts {
		o(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func readBodyStr(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return string(b)
}

func wantStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d (body: %q)", resp.StatusCode, want, b)
	}
}

func TestCreateAndMetadata(t *testing.T) {
	ts := newTestServer(t)

	resp := do(t, ts, "PUT", "/s1", "", hdr("Content-Type", "text/plain"))
	wantStatus(t, resp, 201)
	if got := resp.Header.Get("Stream-Next-Offset"); got != "0000000000000000_0000000000000000" {
		t.Fatalf("next-offset = %q", got)
	}
	if got := resp.Header.Get("Location"); !strings.HasSuffix(got, "/s1") {
		t.Fatalf("location = %q, want suffix /s1", got)
	}
	resp.Body.Close()

	// idempotent create -> 200
	resp = do(t, ts, "PUT", "/s1", "", hdr("Content-Type", "text/plain"))
	wantStatus(t, resp, 200)
	resp.Body.Close()

	// different config -> 409
	resp = do(t, ts, "PUT", "/s1", "", hdr("Content-Type", "application/json"))
	wantStatus(t, resp, 409)
	resp.Body.Close()

	// HEAD
	resp = do(t, ts, "HEAD", "/s1", "")
	wantStatus(t, resp, 200)
	if resp.Header.Get("Stream-Closed") != "" {
		t.Fatalf("unexpected Stream-Closed on open stream")
	}
	resp.Body.Close()

	// HEAD missing -> 404
	resp = do(t, ts, "HEAD", "/nope", "")
	wantStatus(t, resp, 404)
	resp.Body.Close()
}

func TestAppendAndCatchup(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, "PUT", "/s", "", hdr("Content-Type", "text/plain")).Body.Close()

	resp := do(t, ts, "POST", "/s", "hello", hdr("Content-Type", "text/plain"))
	wantStatus(t, resp, 204)
	off := resp.Header.Get("Stream-Next-Offset")
	if off != "0000000000000001_0000000000000000" {
		t.Fatalf("next offset after 1 append = %q", off)
	}
	resp.Body.Close()

	do(t, ts, "POST", "/s", " world", hdr("Content-Type", "text/plain")).Body.Close()

	// catch-up from start
	resp = do(t, ts, "GET", "/s?offset=-1", "")
	wantStatus(t, resp, 200)
	if body := readBodyStr(t, resp); body != "hello world" {
		t.Fatalf("catchup body = %q", body)
	}
	if resp.Header.Get("Stream-Up-To-Date") != "true" {
		t.Fatalf("expected up-to-date")
	}

	// read at tail -> empty, up-to-date, open (no Stream-Closed)
	resp = do(t, ts, "GET", "/s?offset=0000000000000002_0000000000000000", "")
	wantStatus(t, resp, 200)
	if body := readBodyStr(t, resp); body != "" {
		t.Fatalf("tail body = %q", body)
	}
	if resp.Header.Get("Stream-Closed") != "" {
		t.Fatalf("open stream should not report closed")
	}

	// empty POST without close -> 400
	resp = do(t, ts, "POST", "/s", "", hdr("Content-Type", "text/plain"))
	wantStatus(t, resp, 400)
	resp.Body.Close()
}

func TestClosure(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, "PUT", "/c", "", hdr("Content-Type", "text/plain")).Body.Close()
	do(t, ts, "POST", "/c", "final", hdr("Content-Type", "text/plain")).Body.Close()

	// close-only
	resp := do(t, ts, "POST", "/c", "", hdr("Stream-Closed", "true"))
	wantStatus(t, resp, 204)
	if resp.Header.Get("Stream-Closed") != "true" {
		t.Fatalf("close should echo Stream-Closed")
	}
	resp.Body.Close()

	// idempotent close-only -> 204
	resp = do(t, ts, "POST", "/c", "", hdr("Stream-Closed", "true"))
	wantStatus(t, resp, 204)
	resp.Body.Close()

	// append after close -> 409 + Stream-Closed + Stream-Next-Offset
	resp = do(t, ts, "POST", "/c", "more", hdr("Content-Type", "text/plain"))
	wantStatus(t, resp, 409)
	if resp.Header.Get("Stream-Closed") != "true" {
		t.Fatalf("closed append should report Stream-Closed")
	}
	if resp.Header.Get("Stream-Next-Offset") == "" {
		t.Fatalf("closed append should report Stream-Next-Offset")
	}
	resp.Body.Close()

	// EOF discovery at tail: 200 empty + Stream-Closed
	resp = do(t, ts, "GET", "/c?offset=0000000000000001_0000000000000000", "")
	wantStatus(t, resp, 200)
	if resp.Header.Get("Stream-Closed") != "true" {
		t.Fatalf("read at tail of closed stream should report Stream-Closed")
	}
	readBodyStr(t, resp)

	// HEAD reports closed
	resp = do(t, ts, "HEAD", "/c", "")
	if resp.Header.Get("Stream-Closed") != "true" {
		t.Fatalf("HEAD should report closed")
	}
	resp.Body.Close()
}

func TestJSONMode(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, "PUT", "/j", "", hdr("Content-Type", "application/json")).Body.Close()

	// array flattening
	resp := do(t, ts, "POST", "/j", `[{"a":1},{"b":2}]`, hdr("Content-Type", "application/json"))
	wantStatus(t, resp, 204)
	if off := resp.Header.Get("Stream-Next-Offset"); off != "0000000000000002_0000000000000000" {
		t.Fatalf("json batch offset = %q", off)
	}
	resp.Body.Close()

	// single object -> one message
	do(t, ts, "POST", "/j", `{"c":3}`, hdr("Content-Type", "application/json")).Body.Close()

	resp = do(t, ts, "GET", "/j?offset=-1", "")
	wantStatus(t, resp, 200)
	if body := readBodyStr(t, resp); body != `[{"a":1},{"b":2},{"c":3}]` {
		t.Fatalf("json read = %q", body)
	}

	// empty array append -> 400
	resp = do(t, ts, "POST", "/j", `[]`, hdr("Content-Type", "application/json"))
	wantStatus(t, resp, 400)
	resp.Body.Close()

	// invalid json -> 400
	resp = do(t, ts, "POST", "/j", `{bad`, hdr("Content-Type", "application/json"))
	wantStatus(t, resp, 400)
	resp.Body.Close()

	// empty range -> []
	resp = do(t, ts, "GET", "/j?offset=0000000000000003_0000000000000000", "")
	if body := readBodyStr(t, resp); body != `[]` {
		t.Fatalf("empty json range = %q", body)
	}
}

func TestIdempotentProducers(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, "PUT", "/p", "", hdr("Content-Type", "text/plain")).Body.Close()

	prod := func(epoch, seq string) []reqOpt {
		return []reqOpt{
			hdr("Content-Type", "text/plain"),
			hdr("Producer-Id", "prod-1"),
			hdr("Producer-Epoch", epoch),
			hdr("Producer-Seq", seq),
		}
	}

	// first append (epoch 0, seq 0) -> 200
	resp := do(t, ts, "POST", "/p", "a", prod("0", "0")...)
	wantStatus(t, resp, 200)
	resp.Body.Close()

	// duplicate (0,0) -> 204, no new data
	resp = do(t, ts, "POST", "/p", "a", prod("0", "0")...)
	wantStatus(t, resp, 204)
	resp.Body.Close()

	// gap (0,2) -> 409 with expected/received
	resp = do(t, ts, "POST", "/p", "c", prod("0", "2")...)
	wantStatus(t, resp, 409)
	if resp.Header.Get("Producer-Expected-Seq") != "1" || resp.Header.Get("Producer-Received-Seq") != "2" {
		t.Fatalf("gap headers = %q/%q", resp.Header.Get("Producer-Expected-Seq"), resp.Header.Get("Producer-Received-Seq"))
	}
	resp.Body.Close()

	// next in sequence (0,1) -> 200
	resp = do(t, ts, "POST", "/p", "b", prod("0", "1")...)
	wantStatus(t, resp, 200)
	resp.Body.Close()

	// stale epoch after bumping: establish epoch 1
	do(t, ts, "POST", "/p", "d", prod("1", "0")...).Body.Close()
	// zombie at epoch 0 -> 403 with current epoch
	resp = do(t, ts, "POST", "/p", "z", prod("0", "5")...)
	wantStatus(t, resp, 403)
	if resp.Header.Get("Producer-Epoch") != "1" {
		t.Fatalf("stale epoch should echo current epoch 1, got %q", resp.Header.Get("Producer-Epoch"))
	}
	resp.Body.Close()

	// incomplete producer headers -> 400
	resp = do(t, ts, "POST", "/p", "x", hdr("Content-Type", "text/plain"), hdr("Producer-Id", "z"))
	wantStatus(t, resp, 400)
	resp.Body.Close()

	// verify data: a,b (dedup removed the dup), then d
	resp = do(t, ts, "GET", "/p?offset=-1", "")
	if body := readBodyStr(t, resp); body != "abd" {
		t.Fatalf("producer data = %q, want abd", body)
	}
}

func TestStreamSeq(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, "PUT", "/q", "", hdr("Content-Type", "text/plain")).Body.Close()

	resp := do(t, ts, "POST", "/q", "a", hdr("Content-Type", "text/plain"), hdr("Stream-Seq", "001"))
	wantStatus(t, resp, 204)
	resp.Body.Close()

	// regression -> 409
	resp = do(t, ts, "POST", "/q", "b", hdr("Content-Type", "text/plain"), hdr("Stream-Seq", "001"))
	wantStatus(t, resp, 409)
	resp.Body.Close()

	// strictly greater -> ok
	resp = do(t, ts, "POST", "/q", "b", hdr("Content-Type", "text/plain"), hdr("Stream-Seq", "002"))
	wantStatus(t, resp, 204)
	resp.Body.Close()
}

func TestOffsetNow(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, "PUT", "/n", "", hdr("Content-Type", "application/json")).Body.Close()
	do(t, ts, "POST", "/n", `{"old":1}`, hdr("Content-Type", "application/json")).Body.Close()

	resp := do(t, ts, "GET", "/n?offset=now", "")
	wantStatus(t, resp, 200)
	if body := readBodyStr(t, resp); body != "[]" {
		t.Fatalf("offset=now json body = %q", body)
	}
	if resp.Header.Get("Stream-Up-To-Date") != "true" {
		t.Fatalf("offset=now should be up-to-date")
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("offset=now should be no-store")
	}
}

func TestETag(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, "PUT", "/e", "", hdr("Content-Type", "text/plain")).Body.Close()
	do(t, ts, "POST", "/e", "data", hdr("Content-Type", "text/plain")).Body.Close()

	resp := do(t, ts, "GET", "/e?offset=-1", "")
	etag := resp.Header.Get("ETag")
	readBodyStr(t, resp)
	if etag == "" {
		t.Fatalf("missing ETag")
	}
	resp = do(t, ts, "GET", "/e?offset=-1", "", hdr("If-None-Match", etag))
	wantStatus(t, resp, 304)
	resp.Body.Close()
}

func TestLongPoll(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, "PUT", "/l", "", hdr("Content-Type", "text/plain")).Body.Close()

	// timeout with no data -> 204 + up-to-date
	resp := do(t, ts, "GET", "/l?offset=0000000000000000_0000000000000000&live=long-poll", "")
	wantStatus(t, resp, 204)
	if resp.Header.Get("Stream-Up-To-Date") != "true" {
		t.Fatalf("long-poll timeout should be up-to-date")
	}
	if resp.Header.Get("Stream-Cursor") == "" {
		t.Fatalf("long-poll should include Stream-Cursor")
	}
	resp.Body.Close()

	// data arrives during wait -> 200
	go func() {
		time.Sleep(80 * time.Millisecond)
		do(t, ts, "POST", "/l", "live", hdr("Content-Type", "text/plain")).Body.Close()
	}()
	resp = do(t, ts, "GET", "/l?offset=0000000000000000_0000000000000000&live=long-poll", "")
	wantStatus(t, resp, 200)
	if body := readBodyStr(t, resp); body != "live" {
		t.Fatalf("long-poll data = %q", body)
	}

	// closed stream long-poll at tail -> immediate 204 + Stream-Closed
	do(t, ts, "POST", "/l", "", hdr("Stream-Closed", "true")).Body.Close()
	start := time.Now()
	resp = do(t, ts, "GET", "/l?offset=0000000000000001_0000000000000000&live=long-poll", "")
	wantStatus(t, resp, 204)
	if resp.Header.Get("Stream-Closed") != "true" {
		t.Fatalf("closed long-poll should report Stream-Closed")
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("closed long-poll should return immediately, took %v", time.Since(start))
	}
	resp.Body.Close()
}

func TestSSE(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, "PUT", "/sse", "", hdr("Content-Type", "application/json")).Body.Close()
	do(t, ts, "POST", "/sse", `{"n":1}`, hdr("Content-Type", "application/json")).Body.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/sse?offset=-1&live=sse", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sse: %v", err)
	}
	defer resp.Body.Close()
	wantStatus(t, resp, 200)
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("sse content-type = %q", ct)
	}

	sc := bufio.NewScanner(resp.Body)
	var sawData, sawControl bool
	deadline := time.Now().Add(1 * time.Second)
	for sc.Scan() && time.Now().Before(deadline) {
		line := sc.Text()
		if strings.HasPrefix(line, "data:[{\"n\":1}]") {
			sawData = true
		}
		if strings.Contains(line, "streamNextOffset") {
			sawControl = true
			break
		}
	}
	if !sawData || !sawControl {
		t.Fatalf("sse events: data=%v control=%v", sawData, sawControl)
	}
}

func TestDelete(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, "PUT", "/d", "", hdr("Content-Type", "text/plain")).Body.Close()
	do(t, ts, "POST", "/d", "x", hdr("Content-Type", "text/plain")).Body.Close()

	resp := do(t, ts, "DELETE", "/d", "")
	wantStatus(t, resp, 204)
	resp.Body.Close()

	resp = do(t, ts, "GET", "/d?offset=-1", "")
	wantStatus(t, resp, 404)
	resp.Body.Close()

	resp = do(t, ts, "HEAD", "/d", "")
	wantStatus(t, resp, 404)
	resp.Body.Close()

	// delete missing -> 404
	resp = do(t, ts, "DELETE", "/missing", "")
	wantStatus(t, resp, 404)
	resp.Body.Close()
}

func TestCreateClosedWithBody(t *testing.T) {
	ts := newTestServer(t)
	// atomic create-and-close with content
	resp := do(t, ts, "PUT", "/cc", "the answer", hdr("Content-Type", "text/plain"), hdr("Stream-Closed", "true"))
	wantStatus(t, resp, 201)
	if resp.Header.Get("Stream-Closed") != "true" {
		t.Fatalf("create-closed should report Stream-Closed")
	}
	if off := resp.Header.Get("Stream-Next-Offset"); off != "0000000000000001_0000000000000000" {
		t.Fatalf("create-closed offset = %q", off)
	}
	resp.Body.Close()

	resp = do(t, ts, "GET", "/cc?offset=-1", "")
	if body := readBodyStr(t, resp); body != "the answer" {
		t.Fatalf("create-closed body = %q", body)
	}
}
