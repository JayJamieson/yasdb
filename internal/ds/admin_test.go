package ds

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func fetchAdminStreams(t *testing.T, ts *httptest.Server, query string) adminStreamsResponse {
	t.Helper()
	resp, err := http.Get(ts.URL + adminStreamsPath + query)
	if err != nil {
		t.Fatalf("GET %s: %v", adminStreamsPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out adminStreamsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// TestAdminStreamsListing checks that /__admin/streams reports content type,
// closed status, record count, and fork status for a mix of stream shapes.
func TestAdminStreamsListing(t *testing.T) {
	ts, _ := newLiveTestServer(t, Config{}, 5*time.Millisecond)

	do(t, ts, "PUT", "/a", "", hdr("Content-Type", "text/plain")).Body.Close()
	do(t, ts, "POST", "/a", "hello", hdr("Content-Type", "text/plain")).Body.Close()
	do(t, ts, "POST", "/a", "world", hdr("Content-Type", "text/plain")).Body.Close()

	do(t, ts, "PUT", "/b", "", hdr("Content-Type", "text/plain"), hdr("Stream-Closed", "true")).Body.Close()

	do(t, ts, "PUT", "/a-fork", "", hdr("Stream-Forked-From", "/a")).Body.Close()

	got := fetchAdminStreams(t, ts, "")
	if got.NextCursor != "" {
		t.Fatalf("unexpected nextCursor %q for a 3-stream listing under the default limit", got.NextCursor)
	}
	byPath := map[string]adminStreamInfo{}
	for _, s := range got.Streams {
		byPath[s.Path] = s
	}
	if len(byPath) != 3 {
		t.Fatalf("got %d streams, want 3: %+v", len(byPath), got.Streams)
	}

	a := byPath["/a"]
	if a.Records != 2 || a.Closed || a.IsFork {
		t.Fatalf("/a = %+v, want records=2 closed=false isFork=false", a)
	}
	b := byPath["/b"]
	if !b.Closed || b.IsFork {
		t.Fatalf("/b = %+v, want closed=true isFork=false", b)
	}
	fork := byPath["/a-fork"]
	if !fork.IsFork {
		t.Fatalf("/a-fork = %+v, want isFork=true", fork)
	}
}

// TestAdminStreamsPagination walks the cursor across pages smaller than the
// total stream count and checks every stream is seen exactly once.
func TestAdminStreamsPagination(t *testing.T) {
	ts, _ := newLiveTestServer(t, Config{}, 5*time.Millisecond)

	paths := []string{"/p1", "/p2", "/p3", "/p4", "/p5"}
	for _, p := range paths {
		do(t, ts, "PUT", p, "", hdr("Content-Type", "text/plain")).Body.Close()
	}

	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		got := fetchAdminStreams(t, ts, "?limit=2&cursor="+url.QueryEscape(cursor))
		if len(got.Streams) > 2 {
			t.Fatalf("page returned %d streams, want at most 2", len(got.Streams))
		}
		for _, s := range got.Streams {
			if seen[s.Path] {
				t.Fatalf("duplicate stream %s across pages", s.Path)
			}
			seen[s.Path] = true
		}
		pages++
		if got.NextCursor == "" {
			break
		}
		cursor = got.NextCursor
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != len(paths) {
		t.Fatalf("saw %d distinct streams across pages, want %d: %v", len(seen), len(paths), seen)
	}
	if pages != 3 { // 2 + 2 + 1
		t.Fatalf("pages = %d, want 3 for 5 streams at limit=2", pages)
	}
}

// TestAdminStreamsReaderCounts parks one long-poll and one SSE reader on a
// stream and checks the listing reports them under the correct mode.
func TestAdminStreamsReaderCounts(t *testing.T) {
	ts, _ := newLiveTestServer(t, Config{LongPollTimeout: 5 * time.Second, SSELifetime: 5 * time.Second}, 5*time.Millisecond)

	do(t, ts, "PUT", "/r", "", hdr("Content-Type", "text/plain")).Body.Close()

	lpCtx, lpCancel := context.WithCancel(context.Background())
	defer lpCancel()
	lpReq, err := http.NewRequestWithContext(lpCtx, http.MethodGet, ts.URL+"/r?offset=now&live=long-poll", nil)
	if err != nil {
		t.Fatalf("new long-poll request: %v", err)
	}
	go func() {
		resp, err := http.DefaultClient.Do(lpReq)
		if err == nil {
			resp.Body.Close()
		}
	}()

	sseCtx, sseCancel := context.WithCancel(context.Background())
	defer sseCancel()
	sseReq, err := http.NewRequestWithContext(sseCtx, http.MethodGet, ts.URL+"/r?offset=now&live=sse", nil)
	if err != nil {
		t.Fatalf("new sse request: %v", err)
	}
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("sse connect: %v", err)
	}
	defer sseResp.Body.Close()

	var info adminStreamInfo
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := fetchAdminStreams(t, ts, "")
		for _, s := range got.Streams {
			if s.Path == "/r" {
				info = s
			}
		}
		if info.Readers.LongPoll >= 1 && info.Readers.SSE >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if info.Readers.LongPoll != 1 || info.Readers.SSE != 1 {
		t.Fatalf("/r readers = %+v, want longPoll=1 sse=1", info.Readers)
	}
}
