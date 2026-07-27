package ds

import (
	"testing"
	"time"
)

func TestForkInheritsPrefix(t *testing.T) {
	ts := newTestServer(t)
	// source: "first", then "second" (2 records)
	r := do(t, ts, "PUT", "/src", "first", hdr("Content-Type", "text/plain"))
	mid := r.Header.Get("Stream-Next-Offset") // offset after "first"
	r.Body.Close()
	do(t, ts, "POST", "/src", "second", hdr("Content-Type", "text/plain")).Body.Close()

	// fork at mid -> inherits "first" only
	resp := do(t, ts, "PUT", "/fk", "", hdr("Content-Type", "text/plain"), hdr("Stream-Forked-From", "/src"), hdr("Stream-Fork-Offset", mid))
	wantStatus(t, resp, 201)
	resp.Body.Close()
	resp = do(t, ts, "GET", "/fk?offset=-1", "")
	if b := readBodyStr(t, resp); b != "first" {
		t.Fatalf("fork read = %q, want first", b)
	}
}

func TestForkOwnDataAndRecursive(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, "PUT", "/l0", "A", hdr("Content-Type", "text/plain")).Body.Close()

	// l1 forks l0 at tail (default), appends "B"
	wantStatus(t, do(t, ts, "PUT", "/l1", "", hdr("Content-Type", "text/plain"), hdr("Stream-Forked-From", "/l0")), 201)
	do(t, ts, "POST", "/l1", "B", hdr("Content-Type", "text/plain")).Body.Close()
	resp := do(t, ts, "GET", "/l1?offset=-1", "")
	if b := readBodyStr(t, resp); b != "AB" {
		t.Fatalf("l1 = %q, want AB", b)
	}

	// l2 forks l1 at tail, appends "C" -> reads ABC (recursive stitch)
	wantStatus(t, do(t, ts, "PUT", "/l2", "", hdr("Content-Type", "text/plain"), hdr("Stream-Forked-From", "/l1")), 201)
	do(t, ts, "POST", "/l2", "C", hdr("Content-Type", "text/plain")).Body.Close()
	resp = do(t, ts, "GET", "/l2?offset=-1", "")
	if b := readBodyStr(t, resp); b != "ABC" {
		t.Fatalf("l2 = %q, want ABC", b)
	}
}

func TestForkJSON(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, "PUT", "/js", `[{"a":1},{"b":2}]`, hdr("Content-Type", "application/json")).Body.Close()
	// fork inherits content-type (no Content-Type header), at tail, then appends
	wantStatus(t, do(t, ts, "PUT", "/jf", "", hdr("Stream-Forked-From", "/js")), 201)
	do(t, ts, "POST", "/jf", `{"c":3}`, hdr("Content-Type", "application/json")).Body.Close()
	resp := do(t, ts, "GET", "/jf?offset=-1", "")
	if b := readBodyStr(t, resp); b != `[{"a":1},{"b":2},{"c":3}]` {
		t.Fatalf("json fork = %q", b)
	}
}

func TestForkSubOffset(t *testing.T) {
	ts := newTestServer(t)
	// binary sub-offset: "hello" fork at (0,0)+suboffset 3 -> "hel"
	do(t, ts, "PUT", "/bs", "hello", hdr("Content-Type", "text/plain")).Body.Close()
	wantStatus(t, do(t, ts, "PUT", "/bf", "", hdr("Content-Type", "text/plain"),
		hdr("Stream-Forked-From", "/bs"), hdr("Stream-Fork-Offset", "0000000000000000_0000000000000000"),
		hdr("Stream-Fork-Sub-Offset", "3")), 201)
	resp := do(t, ts, "GET", "/bf?offset=-1", "")
	if b := readBodyStr(t, resp); b != "hel" {
		t.Fatalf("binary sub-offset = %q, want hel", b)
	}

	// JSON sub-offset: 4 msgs, fork at (0,0)+suboffset 2 -> first two
	do(t, ts, "PUT", "/js2", `[{"a":1},{"b":2},{"c":3},{"d":4}]`, hdr("Content-Type", "application/json")).Body.Close()
	wantStatus(t, do(t, ts, "PUT", "/jf2", "", hdr("Content-Type", "application/json"),
		hdr("Stream-Forked-From", "/js2"), hdr("Stream-Fork-Offset", "0000000000000000_0000000000000000"),
		hdr("Stream-Fork-Sub-Offset", "2")), 201)
	resp = do(t, ts, "GET", "/jf2?offset=-1", "")
	if b := readBodyStr(t, resp); b != `[{"a":1},{"b":2}]` {
		t.Fatalf("json sub-offset = %q", b)
	}
}

func TestForkSoftDeleteLifecycle(t *testing.T) {
	ts := newTestServerCfg(t, Config{SweepInterval: time.Hour}) // no sweeper interference
	do(t, ts, "PUT", "/sd", "preserved", hdr("Content-Type", "text/plain")).Body.Close()
	wantStatus(t, do(t, ts, "PUT", "/sdf", "", hdr("Stream-Forked-From", "/sd"), hdr("Content-Type", "text/plain")), 201)

	// DELETE source while fork exists -> 204 soft-delete
	wantStatus(t, do(t, ts, "DELETE", "/sd", ""), 204)
	// source path now 410
	wantStatus(t, do(t, ts, "HEAD", "/sd", ""), 410)
	wantStatus(t, do(t, ts, "GET", "/sd?offset=-1", ""), 410)
	wantStatus(t, do(t, ts, "DELETE", "/sd", ""), 410)
	// re-create blocked
	wantStatus(t, do(t, ts, "PUT", "/sd", "", hdr("Content-Type", "text/plain")), 409)
	// fork still reads the inherited data
	resp := do(t, ts, "GET", "/sdf?offset=-1", "")
	if b := readBodyStr(t, resp); b != "preserved" {
		t.Fatalf("fork after source soft-delete = %q", b)
	}

	// delete the fork -> cascade GC the soft-deleted source; source path free again (404)
	wantStatus(t, do(t, ts, "DELETE", "/sdf", ""), 204)
	// give background cleanup a moment
	time.Sleep(150 * time.Millisecond)
	wantStatus(t, do(t, ts, "GET", "/sd?offset=-1", ""), 404)
}

func TestForkErrors(t *testing.T) {
	ts := newTestServer(t)
	// fork from nonexistent source -> 404
	wantStatus(t, do(t, ts, "PUT", "/f1", "", hdr("Stream-Forked-From", "/nope")), 404)
	// content-type mismatch -> 409
	do(t, ts, "PUT", "/ct", "x", hdr("Content-Type", "text/plain")).Body.Close()
	wantStatus(t, do(t, ts, "PUT", "/f2", "", hdr("Stream-Forked-From", "/ct"), hdr("Content-Type", "application/json")), 409)
	// fork offset beyond tail -> 400
	wantStatus(t, do(t, ts, "PUT", "/f3", "", hdr("Content-Type", "text/plain"), hdr("Stream-Forked-From", "/ct"), hdr("Stream-Fork-Offset", "0000000000000099_0000000000000000")), 400)
	// sub-offset without forked-from -> 400
	wantStatus(t, do(t, ts, "PUT", "/f4", "", hdr("Content-Type", "text/plain"), hdr("Stream-Fork-Sub-Offset", "0")), 400)
	// idempotent re-create of identical fork -> 200
	do(t, ts, "PUT", "/base", "b", hdr("Content-Type", "text/plain")).Body.Close()
	wantStatus(t, do(t, ts, "PUT", "/fi", "", hdr("Content-Type", "text/plain"), hdr("Stream-Forked-From", "/base")), 201)
	wantStatus(t, do(t, ts, "PUT", "/fi", "", hdr("Content-Type", "text/plain"), hdr("Stream-Forked-From", "/base")), 200)
}
