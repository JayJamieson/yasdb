package ds

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func postBulkProvision(t *testing.T, ts *httptest.Server, srv *Server, req bulkProvisionRequest) bulkProvisionResponse {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	rr := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/__admin/bulk-provision", bytes.NewReader(body))
	srv.HandleBulkProvision(rr, httpReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("HandleBulkProvision status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var out bulkProvisionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

// TestBulkProvisionCreatesUsableStreams checks that streams created via bulk
// provisioning are indistinguishable from PUT-created ones: listed correctly,
// and immediately appendable.
func TestBulkProvisionCreatesUsableStreams(t *testing.T) {
	ts := newTestServer(t)
	srv := ts.Config.Handler.(*Server)

	out := postBulkProvision(t, ts, srv, bulkProvisionRequest{
		PathPrefix:  "/bulk/s",
		Count:       10,
		ContentType: "text/plain",
	})
	if out.Created != 10 {
		t.Fatalf("Created = %d, want 10", out.Created)
	}
	if out.Batches != 1 {
		t.Fatalf("Batches = %d, want 1 (count fits in one default-sized batch)", out.Batches)
	}

	listing := fetchAdminStreams(t, ts, "?limit=500")
	byPath := map[string]adminStreamInfo{}
	for _, s := range listing.Streams {
		byPath[s.Path] = s
	}
	for i := 0; i < 10; i++ {
		path := "/bulk/s" + strconv.Itoa(i)
		info, ok := byPath[path]
		if !ok {
			t.Fatalf("stream %q missing from listing", path)
		}
		if info.ContentType != "text/plain" {
			t.Errorf("%s: ContentType = %q, want text/plain", path, info.ContentType)
		}
		if info.Records != 0 {
			t.Errorf("%s: Records = %d, want 0", path, info.Records)
		}
	}

	// A bulk-provisioned stream must accept an append exactly like a
	// PUT-created one — this is the real correctness bar, not just presence
	// in the listing.
	resp := do(t, ts, "POST", "/bulk/s0", "hello", hdr("Content-Type", "text/plain"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("append to bulk-provisioned stream: status = %d", resp.StatusCode)
	}
}

// TestBulkProvisionBatching checks that a small BatchSize splits the request
// into multiple Commit batches without gaps, duplicates, or id collisions
// across the batch boundary.
func TestBulkProvisionBatching(t *testing.T) {
	ts := newTestServer(t)
	srv := ts.Config.Handler.(*Server)

	out := postBulkProvision(t, ts, srv, bulkProvisionRequest{
		PathPrefix: "/batched/s",
		Count:      25,
		BatchSize:  10,
	})
	if out.Created != 25 {
		t.Fatalf("Created = %d, want 25", out.Created)
	}
	if out.Batches != 3 {
		t.Fatalf("Batches = %d, want 3 (10+10+5)", out.Batches)
	}

	listing := fetchAdminStreams(t, ts, "?limit=500")
	seen := map[string]bool{}
	for _, s := range listing.Streams {
		seen[s.Path] = true
	}
	for i := 0; i < 25; i++ {
		if !seen["/batched/s"+strconv.Itoa(i)] {
			t.Errorf("stream /batched/s%d missing after batched provisioning", i)
		}
	}
}

func TestBulkProvisionValidation(t *testing.T) {
	ts := newTestServer(t)
	srv := ts.Config.Handler.(*Server)

	cases := []struct {
		name string
		req  bulkProvisionRequest
	}{
		{"missing prefix", bulkProvisionRequest{Count: 5}},
		{"zero count", bulkProvisionRequest{PathPrefix: "/x"}},
		{"negative count", bulkProvisionRequest{PathPrefix: "/x", Count: -1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body, _ := json.Marshal(c.req)
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/__admin/bulk-provision", bytes.NewReader(body))
			srv.HandleBulkProvision(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rr.Code)
			}
		})
	}

	t.Run("wrong method", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/__admin/bulk-provision", nil)
		srv.HandleBulkProvision(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rr.Code)
		}
	})
}
