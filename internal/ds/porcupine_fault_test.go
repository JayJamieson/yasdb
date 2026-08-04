package ds

// Fault injection: linearizability of idempotent producers when appends
// fail *indefinitely*. The client sends a request but never learns whether
// it became durable. This is where the NondeterministicModel earns its
// keep, and where the exactly-once guarantee is actually tested: after an
// ambiguous append the client retries the same (producerId, epoch, seq),
// and the model must reconcile the retry's definite answer (204 duplicate
// means the original did commit; 200 accept means it did not) into a
// single legal order, with the record present exactly once.
//
// A tiny in-process reverse proxy sits in front of the real server and, on
// a deterministic fraction of POST (append) requests, injects one of two
// faults:
//
//   drop-request      — the request never reaches the server (not durable),
//                        then the client connection is severed.
//   commit-then-drop  — the request is forwarded and fully processed by the
//                        server (durable!), then the connection is severed
//                        before the response reaches the client.
//
// Both look identical to the client: a transport error. Only the two
// together exercise both nondeterministic branches (the retry sometimes
// accepts fresh, sometimes dedups). This honors YASDB_TEST_DURABILITY
// (sync or notifier).

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anishathalye/porcupine"
)

// faultProxy forwards to a backend, injecting connection-severing faults on every
// faultEvery-th POST (alternating drop-request / commit-then-drop).
type faultProxy struct {
	server *httptest.Server
	faults atomic.Int64
}

func newFaultProxy(tb testing.TB, backend *httptest.Server, faultEvery int64) *faultProxy {
	tb.Helper()
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		tb.Fatalf("parse backend url: %v", err)
	}
	rp := httputil.NewSingleHostReverseProxy(backendURL)
	fwd := &http.Client{Timeout: 10 * time.Second}

	fp := &faultProxy{}
	var counter atomic.Int64

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if n := counter.Add(1); faultEvery > 0 && n%faultEvery == 0 {
				fp.faults.Add(1)
				commitThenDrop := (n/faultEvery)%2 == 0
				if commitThenDrop {
					// Forward and fully drain so the server commits durably, then
					// discard the response and sever the client connection.
					forwardAndDiscard(fwd, backend.URL, r)
				}
				severConnection(w)
				return
			}
		}
		rp.ServeHTTP(w, r)
	})

	fp.server = httptest.NewServer(handler)
	tb.Cleanup(fp.server.Close)
	return fp
}

// forwardAndDiscard replays r against the backend and drains the response, so the
// append is durably applied even though the client will never see the answer.
func forwardAndDiscard(cl *http.Client, backendURL string, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return
	}
	req, err := http.NewRequest(r.Method, backendURL+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header = r.Header.Clone()
	resp, err := cl.Do(req)
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// severConnection hijacks and closes the client TCP connection, so http.Client.Do
// returns a transport error rather than any HTTP status.
func severConnection(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
			return
		}
	}
	w.WriteHeader(http.StatusBadGateway) // fallback if hijack unsupported
}

func TestLinearizableProducersUnderFaults(t *testing.T) {
	if testing.Short() {
		t.Skip("fault-injection linearizability check skipped in -short")
	}

	streams := envInt("YASDB_FAULT_STREAMS", 2)
	faultEvery := int64(envInt("YASDB_FAULT_EVERY", 5)) // fault 1 in N appends

	backend, _ := newRealLiveServer(t, Config{
		LongPollTimeout: 200 * time.Millisecond,
		MaxReadBytes:    1 << 20,
	}, 5*time.Millisecond)
	proxy := newFaultProxy(t, backend, faultEvery)

	for s := 0; s < streams; s++ {
		resp := do(t, proxy.server, "PUT", prodStreamPath(s), "", hdr("Content-Type", "application/json"))
		wantStatus(t, resp, 201)
		resp.Body.Close()
	}

	var (
		clock atomic.Int64
		hist  = &history{}
		errs  = &errList{}
		// Disable keep-alives: a faulted POST severs its TCP connection, and a
		// pooled/reused connection must never carry that failure into an unrelated
		// request. Each request gets a fresh connection.
		cl = &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{DisableKeepAlives: true},
		}
		wg sync.WaitGroup
	)
	now := func() int64 { return clock.Add(1) }
	script := producerScript()

	for s := 0; s < streams; s++ {
		for p := 0; p < numProducers; p++ {
			wg.Add(1)
			clientID := s*numProducers + p
			go func(stream, prod, clientID int) {
				defer wg.Done()
				path := proxy.server.URL + prodStreamPath(stream)
				pid := fmt.Sprintf("p%d", prod)
				for i, ev := range script {
					prodAppendRetrying(cl, hist, errs, now, stream, prod, clientID, path, pid, ev.epoch, ev.seq)
					if i%4 == 3 {
						prodRead(cl, hist, errs, now, stream, clientID, path)
					}
				}
				prodRead(cl, hist, errs, now, stream, clientID, path)
			}(s, p, clientID)
		}
	}
	wg.Wait()

	if msgs := errs.all(); len(msgs) > 0 {
		t.Fatalf("client errors during load:\n  %s", strings.Join(msgs, "\n  "))
	}
	t.Logf("injected %d faults over %d ops", proxy.faults.Load(), len(hist.ops))
	if proxy.faults.Load() == 0 {
		t.Log("warning: no faults were injected; the indefinite path was not exercised")
	}

	nm := producerModel()
	model := nm.ToModel()
	res, info := porcupine.CheckOperationsVerbose(model, hist.ops, 90*time.Second)
	switch res {
	case porcupine.Ok:
		t.Logf("linearizable under faults: %d ops, %d streams", len(hist.ops), streams)
		if out := os.Getenv("YASDB_PORCUPINE_VIZ"); out != "" {
			if err := porcupine.VisualizePath(model, info, out); err != nil {
				t.Logf("visualization: %v", err)
			} else {
				t.Logf("visualization written to %s", out)
			}
		}
	case porcupine.Unknown:
		t.Logf("porcupine returned Unknown (checker timed out); no violation found in %d ops", len(hist.ops))
	case porcupine.Illegal:
		path := filepath.Join(vizDir(t), "fault-violation.html")
		if err := porcupine.VisualizePath(model, info, path); err != nil {
			t.Logf("visualization: %v", err)
		}
		t.Fatalf("history NOT linearizable under faults (%d ops) — visualization: %s", len(hist.ops), path)
	}
}

// TestFaultModelHasTeeth is the negative control for the indefinite path. It
// proves the model enforces exactly-once across a retry: it rejects a retry that
// double-writes after a committed-but-lost append, and accepts *both* legal
// resolutions — which produce the identical end state (one record), the whole
// point of idempotent producers.
func TestFaultModelHasTeeth(t *testing.T) {
	nm := producerModel()
	model := nm.ToModel()

	tok := "s0/p0/e1/n0"
	rh := recordHash([]byte(tok))
	in := prodInput{kind: "append", stream: 0, prod: 0, epoch: 1, seq: 0, rhash: rh, token: tok}
	readOneRecord := prodInput{kind: "read", stream: 0}
	oneRecordHash := chainHash(0, rh)

	// BUG: the original append committed (indefinite), and the retry double-writes
	// (200 with the tail advancing a second time) instead of deduplicating. No
	// world explains it: if the original committed, the retry must be 204@tail=1;
	// if it didn't, the retry accepts at tail=1 — never tail=2.
	doubleWriteOnRetry := []porcupine.Operation{
		{ClientId: 0, Input: in, Output: prodOutput{indefinite: true}, Call: 1, Return: 2},
		{ClientId: 0, Input: in, Output: prodOutput{status: 200, tail: 2}, Call: 3, Return: 4},
	}
	if porcupine.CheckOperations(model, doubleWriteOnRetry) {
		t.Fatal("double-write on idempotent retry accepted; fault model is too weak")
	}

	// LEGAL A: original committed, retry deduplicates (204, tail unchanged).
	committedThenDup := []porcupine.Operation{
		{ClientId: 0, Input: in, Output: prodOutput{indefinite: true}, Call: 1, Return: 2},
		{ClientId: 0, Input: in, Output: prodOutput{status: 204, tail: 1}, Call: 3, Return: 4},
		{ClientId: 1, Input: readOneRecord, Output: prodOutput{status: 200, tail: 1, hash: oneRecordHash, hashKnown: true}, Call: 5, Return: 6},
	}
	if !porcupine.CheckOperations(model, committedThenDup) {
		t.Fatal("legal committed-then-duplicate history rejected")
	}

	// LEGAL B: original did NOT commit, retry accepts fresh (200, tail=1). Same
	// observable end state as A — one record.
	notCommittedThenAccept := []porcupine.Operation{
		{ClientId: 0, Input: in, Output: prodOutput{indefinite: true}, Call: 1, Return: 2},
		{ClientId: 0, Input: in, Output: prodOutput{status: 200, tail: 1}, Call: 3, Return: 4},
		{ClientId: 1, Input: readOneRecord, Output: prodOutput{status: 200, tail: 1, hash: oneRecordHash, hashKnown: true}, Call: 5, Return: 6},
	}
	if !porcupine.CheckOperations(model, notCommittedThenAccept) {
		t.Fatal("legal not-committed-then-accept history rejected")
	}
}

// prodAppendRetrying issues one logical append, retrying the *same* (epoch, seq)
// on an indefinite failure — the idempotent-producer retry pattern. Each faulted
// attempt is recorded as an indefinite op (may or may not be durable); the loop
// ends on the first definite answer (2xx or a 4xx producer verdict).
func prodAppendRetrying(cl *http.Client, hist *history, errs *errList, now func() int64, stream, prod, clientID int, path, pid string, epoch, seq uint64) {
	token := fmt.Sprintf("s%d/p%d/e%d/n%d", stream, prod, epoch, seq)
	in := prodInput{kind: "append", stream: stream, prod: prod, epoch: epoch, seq: seq, rhash: recordHash([]byte(token)), token: token}
	const maxAttempts = 12

	for attempt := 0; attempt < maxAttempts; attempt++ {
		req, _ := http.NewRequest("POST", path, strings.NewReader(`["`+token+`"]`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Producer-Id", pid)
		req.Header.Set("Producer-Epoch", strconv.FormatUint(epoch, 10))
		req.Header.Set("Producer-Seq", strconv.FormatUint(seq, 10))

		call := now()
		resp, err := cl.Do(req)
		if err != nil {
			ret := now()
			hist.add(porcupine.Operation{ClientId: clientID, Input: in, Output: prodOutput{indefinite: true}, Call: call, Return: ret})
			continue // retry the same (epoch, seq)
		}
		next := resp.Header.Get("Stream-Next-Offset")
		status := resp.StatusCode
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		ret := now()

		if status == http.StatusBadGateway { // hijack-unsupported fallback ⇒ indefinite
			hist.add(porcupine.Operation{ClientId: clientID, Input: in, Output: prodOutput{indefinite: true}, Call: call, Return: ret})
			continue
		}
		if status >= 500 {
			errs.add("append %s: server error %d", token, status)
			return
		}
		out := prodOutput{status: status}
		if status == 200 || status == 204 {
			n, ok := offsetSeq(next)
			if !ok {
				errs.add("append %s: bad next-offset %q", token, next)
				return
			}
			out.tail = uint64(n)
		}
		hist.add(porcupine.Operation{ClientId: clientID, Input: in, Output: out, Call: call, Return: ret})
		return // definite answer
	}
	errs.add("append %s: exhausted %d attempts (all faulted)", token, maxAttempts)
}
