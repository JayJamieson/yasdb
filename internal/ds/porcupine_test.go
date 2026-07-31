package ds

// Linearizability checking with Porcupine (github.com/anishathalye/porcupine).
//
// Each stream is modelled as an append-only log — an independent linearizable
// object, so the history is partitioned per stream (the classic register-per-key
// pattern). Many clients concurrently append unique values and periodically read
// the whole log back. Porcupine then searches for a single total order of the
// appends that is consistent with (a) real-time: an op's linearization point
// lies within its [call, return] interval, and (b) the observed outputs:
//
//   - append(v) returns Stream-Next-Offset, whose seq must equal len(log)+1 in
//     that order — this pins the offset to a dense, gapless 1..N with no
//     duplicates and no lost/torn appends;
//   - read() returns the full JSON array, which must equal the log at its
//     linearization point — this catches reordering, dirty reads, and stale tails.
//
// yasdb serialises appends per stream through a single streamer goroutine, so
// this should always be linearizable; the test is a differential guard on the
// concurrency + durability machinery (run it in both durability modes via
// YASDB_TEST_DURABILITY=notifier). On a violation it writes an interactive HTML
// visualization and fails with its path.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// --- model -----------------------------------------------------------------

// opInput/opOutput are the recorded input/output of one HTTP operation.
type opInput struct {
	kind   string // "append" | "read"
	stream int
	value  string // append payload (append only)
}

type opOutput struct {
	seq    int      // append: seq component of the returned Stream-Next-Offset
	values []string // read: the decoded JSON array
}

// plog is the per-stream model state: the ordered log, kept as a separator-
// joined string plus its length so the whole value is comparable (Porcupine
// keys visited states in a map, so state must be hashable).
type plog struct {
	joined string
	n      int
}

const logSep = "\x1f" // unit separator; never appears in a value

func (s plog) append(v string) plog {
	if s.n == 0 {
		return plog{joined: v, n: 1}
	}
	return plog{joined: s.joined + logSep + v, n: s.n + 1}
}

func (s plog) records() []string {
	if s.n == 0 {
		return nil
	}
	return strings.Split(s.joined, logSep)
}

func streamLogModel() porcupine.Model {
	return porcupine.Model{
		// Partition the history per stream: each stream is checked independently.
		Partition: func(history []porcupine.Operation) [][]porcupine.Operation {
			byStream := map[int][]porcupine.Operation{}
			order := []int{}
			for _, op := range history {
				s := op.Input.(opInput).stream
				if _, seen := byStream[s]; !seen {
					order = append(order, s)
				}
				byStream[s] = append(byStream[s], op)
			}
			parts := make([][]porcupine.Operation, 0, len(order))
			for _, s := range order {
				parts = append(parts, byStream[s])
			}
			return parts
		},
		Init: func() interface{} { return plog{} },
		Step: func(state, input, output interface{}) (bool, interface{}) {
			s := state.(plog)
			in := input.(opInput)
			out := output.(opOutput)
			switch in.kind {
			case "append":
				// The k-th append in the chosen order returns seq k = len+1.
				if out.seq != s.n+1 {
					return false, state
				}
				return true, s.append(in.value)
			case "read":
				if !equalStrings(out.values, s.records()) {
					return false, state
				}
				return true, state // reads don't mutate
			}
			return false, state
		},
		Equal: func(a, b interface{}) bool { return a.(plog) == b.(plog) },
		DescribeOperation: func(input, output interface{}) string {
			in := input.(opInput)
			out := output.(opOutput)
			if in.kind == "append" {
				return fmt.Sprintf("append(%s) -> seq %d", in.value, out.seq)
			}
			return fmt.Sprintf("read() -> [%d]", len(out.values))
		},
		DescribeState: func(state interface{}) string {
			return fmt.Sprintf("len=%d", state.(plog).n)
		},
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- test ------------------------------------------------------------------

func TestLinearizableStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("linearizability check skipped in -short")
	}

	streams := envInt("YASDB_LIN_STREAMS", 4)
	clients := envInt("YASDB_LIN_CLIENTS", 4) // per stream
	opsPer := envInt("YASDB_LIN_OPS", 50)     // ops per client
	const readEvery = 6                       // one read every N ops per client

	// Honors YASDB_TEST_DURABILITY (sync or notifier); both provide strict
	// linearizability. (Notifier's read-after-ack ordering is regression-tested by
	// TestProbeReadAfterAck.)
	ts, _ := newRealLiveServer(t, Config{
		LongPollTimeout: 200 * time.Millisecond,
		MaxReadBytes:    1 << 20,
	}, 5*time.Millisecond)

	// Create the JSON streams up front.
	for s := 0; s < streams; s++ {
		resp := do(t, ts, "PUT", streamPath(s), "", hdr("Content-Type", "application/json"))
		wantStatus(t, resp, 201)
		resp.Body.Close()
	}

	var (
		clock atomic.Int64 // logical clock: strictly increasing over real events
		hist  = &history{}
		errs  = &errList{}
		cl    = &http.Client{Timeout: 15 * time.Second}
		wg    sync.WaitGroup
	)
	now := func() int64 { return clock.Add(1) }

	for s := 0; s < streams; s++ {
		for c := 0; c < clients; c++ {
			wg.Add(1)
			clientID := s*clients + c
			go func(stream, clientID, seed int) {
				defer wg.Done()
				path := ts.URL + streamPath(stream)
				for k := 0; k < opsPer; k++ {
					if k > 0 && k%readEvery == 0 {
						doRead(cl, hist, errs, now, stream, clientID, path)
						continue
					}
					value := fmt.Sprintf("s%d-c%d-%d", stream, clientID, k)
					doAppend(cl, hist, errs, now, stream, clientID, path, value)
				}
			}(s, clientID, s*clients+c)
		}
	}
	wg.Wait()

	if msgs := errs.all(); len(msgs) > 0 {
		t.Fatalf("client errors during load:\n  %s", strings.Join(msgs, "\n  "))
	}

	model := streamLogModel()
	res, info := porcupine.CheckOperationsVerbose(model, hist.ops, 30*time.Second)

	switch res {
	case porcupine.Ok:
		t.Logf("linearizable: %d ops across %d streams, %d clients (durability=%q)",
			len(hist.ops), streams, streams*clients, effectiveDurability())
	case porcupine.Unknown:
		t.Logf("porcupine returned Unknown (checker timed out); no violation found in %d ops", len(hist.ops))
	case porcupine.Illegal:
		path := filepath.Join(vizDir(t), "linearizability-violation.html")
		if err := porcupine.VisualizePath(model, info, path); err != nil {
			t.Logf("failed to write visualization: %v", err)
		}
		t.Fatalf("NOT linearizable across %d ops — visualization: %s", len(hist.ops), path)
	}

	// Optional: always emit a visualization when YASDB_PORCUPINE_VIZ names a file
	// (handy for eyeballing a passing run).
	if out := os.Getenv("YASDB_PORCUPINE_VIZ"); out != "" && res != porcupine.Illegal {
		if err := porcupine.VisualizePath(model, info, out); err != nil {
			t.Logf("visualization: %v", err)
		} else {
			t.Logf("visualization written to %s", out)
		}
	}
}

// TestLinearizabilityModelHasTeeth is a negative control: it feeds the stream-log
// model a hand-built history that is NOT linearizable and asserts Porcupine
// rejects it. Without this, a passing TestLinearizableStreams proves nothing —
// a model that accepts everything would also pass.
func TestLinearizabilityModelHasTeeth(t *testing.T) {
	model := streamLogModel()

	// Two appends on the same stream, sequential in real time (A returns at t=2,
	// B is called at t=3), yet both claim Stream-Next-Offset seq 1. Any order
	// must assign seq 1 then seq 2, so a duplicate seq 1 is impossible.
	dupSeq := []porcupine.Operation{
		{ClientId: 0, Input: opInput{kind: "append", stream: 0, value: "a"}, Output: opOutput{seq: 1}, Call: 1, Return: 2},
		{ClientId: 1, Input: opInput{kind: "append", stream: 0, value: "b"}, Output: opOutput{seq: 1}, Call: 3, Return: 4},
	}
	if porcupine.CheckOperations(model, dupSeq) {
		t.Fatal("duplicate-offset history was accepted; model is too weak")
	}

	// A read that misses an already-returned append (dirty/stale read): append
	// "a" fully returns, then a later read comes back empty.
	staleRead := []porcupine.Operation{
		{ClientId: 0, Input: opInput{kind: "append", stream: 0, value: "a"}, Output: opOutput{seq: 1}, Call: 1, Return: 2},
		{ClientId: 1, Input: opInput{kind: "read", stream: 0}, Output: opOutput{values: nil}, Call: 3, Return: 4},
	}
	if porcupine.CheckOperations(model, staleRead) {
		t.Fatal("stale-read history was accepted; model is too weak")
	}

	// Sanity: the legal version of each is accepted, and the visualization path
	// works end to end.
	legal := []porcupine.Operation{
		{ClientId: 0, Input: opInput{kind: "append", stream: 0, value: "a"}, Output: opOutput{seq: 1}, Call: 1, Return: 2},
		{ClientId: 1, Input: opInput{kind: "read", stream: 0}, Output: opOutput{values: []string{"a"}}, Call: 3, Return: 4},
	}
	res, info := porcupine.CheckOperationsVerbose(model, legal, 5*time.Second)
	if res != porcupine.Ok {
		t.Fatalf("legal history rejected: %v", res)
	}
	if err := porcupine.VisualizePath(model, info, filepath.Join(t.TempDir(), "ok.html")); err != nil {
		t.Fatalf("visualize: %v", err)
	}
}

// --- op drivers (record call/return around each HTTP request) ---------------

func doAppend(cl *http.Client, hist *history, errs *errList, now func() int64, stream, clientID int, path, value string) {
	body := strings.NewReader(`["` + value + `"]`)
	req, _ := http.NewRequest("POST", path, body)
	req.Header.Set("Content-Type", "application/json")

	call := now()
	resp, err := cl.Do(req)
	if err != nil {
		errs.add("append %s: %v", value, err)
		return
	}
	next := resp.Header.Get("Stream-Next-Offset")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	ret := now()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		errs.add("append %s: status %d", value, resp.StatusCode)
		return
	}
	seq, ok := offsetSeq(next)
	if !ok {
		errs.add("append %s: bad next-offset %q", value, next)
		return
	}
	hist.add(porcupine.Operation{
		ClientId: clientID,
		Input:    opInput{kind: "append", stream: stream, value: value},
		Output:   opOutput{seq: seq},
		Call:     call,
		Return:   ret,
	})
}

func doRead(cl *http.Client, hist *history, errs *errList, now func() int64, stream, clientID int, path string) {
	req, _ := http.NewRequest("GET", path+"?offset=-1", nil)

	call := now()
	resp, err := cl.Do(req)
	if err != nil {
		errs.add("read: %v", err)
		return
	}
	upToDate := resp.Header.Get("Stream-Up-To-Date")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	ret := now()

	if resp.StatusCode != http.StatusOK {
		errs.add("read: status %d", resp.StatusCode)
		return
	}
	// The history is sized to fit under MaxReadBytes, so one read reaches the
	// tail. If it ever didn't, the array would be a prefix and over-constrain
	// the model — surface that loudly rather than record a partial read.
	if upToDate != "true" {
		errs.add("read: not up-to-date (history too large for one read?)")
		return
	}
	values, err := decodeJSONArray(b)
	if err != nil {
		errs.add("read: decode %q: %v", string(b), err)
		return
	}
	hist.add(porcupine.Operation{
		ClientId: clientID,
		Input:    opInput{kind: "read", stream: stream},
		Output:   opOutput{values: values},
		Call:     call,
		Return:   ret,
	})
}

// --- small helpers ----------------------------------------------------------

func streamPath(s int) string { return fmt.Sprintf("/lin/s%d", s) }

// offsetSeq parses the seq component of a "<seq>_<byte>" offset.
func offsetSeq(off string) (int, bool) {
	i := strings.IndexByte(off, '_')
	if i < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(off[:i])
	if err != nil {
		return 0, false
	}
	return n, true
}

func decodeJSONArray(b []byte) ([]string, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var arr []string
	if err := json.Unmarshal(b, &arr); err != nil {
		return nil, err
	}
	return arr, nil
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func effectiveDurability() string {
	if d := testDurability(); d != "" {
		return d
	}
	return "sync"
}

func vizDir(t *testing.T) string {
	if d := os.Getenv("YASDB_PORCUPINE_VIZ_DIR"); d != "" {
		return d
	}
	return t.TempDir()
}

type history struct {
	mu  sync.Mutex
	ops []porcupine.Operation
}

func (h *history) add(op porcupine.Operation) {
	h.mu.Lock()
	h.ops = append(h.ops, op)
	h.mu.Unlock()
}

type errList struct {
	mu   sync.Mutex
	msgs []string
}

func (e *errList) add(format string, a ...any) {
	e.mu.Lock()
	e.msgs = append(e.msgs, fmt.Sprintf(format, a...))
	e.mu.Unlock()
}

func (e *errList) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.msgs
}
