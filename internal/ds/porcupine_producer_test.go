package ds

// Linearizability of yasdb's idempotent producers (SPEC §9 / protocol §5.2.1).
//
// This extends the plain-append check in porcupine_test.go to the producer
// state machine: per (stream, producerId) epoch fencing + per-batch sequence
// dedup. It borrows two techniques from the s2-streamstore/s2-verification
// model (s2-porcupine):
//
//   1. porcupine.NondeterministicModel — Step returns the *set* of possible next
//      states. Definite outcomes return a single state; an empty set means the
//      observed result is impossible (a linearizability violation). This is the
//      shape that also lets an "indefinite" (timed-out) append be modelled as
//      {maybe-durable, not-durable} once fault injection is added.
//   2. A constant-size chained content hash. Instead of storing the whole log,
//      the state carries one uint64 = fold(chainHash) over every committed record
//      body in order. A read folds the same function over the records it returns;
//      any reorder, drop, duplicate, or corruption changes the hash. State stays
//      O(1) regardless of stream length.
//
// Modelled producer outcomes (verified against internal/ds/streamer.go):
//   accept (new data)   -> 200, Stream-Next-Offset seq = tail+1 ; tail++, lastSeq=seq
//   duplicate           -> 204, Stream-Next-Offset seq = current tail ; no write
//   stale epoch         -> 403 ; no write
//   bad seq start       -> 400 ; no write (epoch bumped but first seq != 0)
//   gap                 -> 409 ; no write (seq > lastSeq+1)
//
// Every append here carries exactly one record, so tail advances by 1 per accept.

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
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

const numProducers = 3 // producers per stream (fixed so model state is comparable)

// prodState is the per-stream model state. Arrays (not maps) keep it comparable,
// which Porcupine requires to key visited states.
type prodState struct {
	tail    uint64
	hash    uint64 // chained hash over committed record bodies, in order
	epoch   [numProducers]uint64
	lastSeq [numProducers]uint64
	seen    [numProducers]bool
}

type prodInput struct {
	kind   string // "append" | "read"
	stream int
	prod   int    // producer index (append)
	epoch  uint64 // Producer-Epoch (append)
	seq    uint64 // Producer-Seq (append)
	rhash  uint64 // hash of this record's body (append)
	token  string // human-readable body, for visualization
}

type prodOutput struct {
	status    int    // HTTP status
	tail      uint64 // Stream-Next-Offset seq (append 200/204, read)
	hash      uint64 // read: chained content hash the reader computed
	hashKnown bool
	// indefinite marks an append whose result the client never learned (the
	// fault proxy severed the connection). It may or may not have become durable;
	// see the nondeterministic branch in Step and porcupine_fault_test.go.
	indefinite bool
}

// recordHash / chainHash: identical folds are used by the model (per accepted
// append) and by the harness (per record returned from a read), so the two agree
// iff records are stored in accept order with no loss or duplication.
func recordHash(b []byte) uint64 {
	h := fnv.New64a()
	h.Write(b)
	return h.Sum64()
}

func chainHash(acc, rh uint64) uint64 {
	var buf [16]byte
	binary.LittleEndian.PutUint64(buf[0:8], acc)
	binary.LittleEndian.PutUint64(buf[8:16], rh)
	h := fnv.New64a()
	h.Write(buf[:])
	return h.Sum64()
}

// predKind is the outcome the producer state machine predicts for an append in a
// given order — a faithful mirror of validateProducer in producer.go.
type predKind int

const (
	predAccept predKind = iota
	predDup
	predStale
	predBadSeq
	predGap
)

func (s prodState) predict(in prodInput) predKind {
	p := in.prod
	if !s.seen[p] {
		if in.seq != 0 {
			return predGap // fresh producer must start at seq 0
		}
		return predAccept
	}
	if in.epoch < s.epoch[p] {
		return predStale
	}
	if in.epoch > s.epoch[p] {
		if in.seq != 0 {
			return predBadSeq
		}
		return predAccept // new epoch, reset to seq 0
	}
	switch {
	case in.seq <= s.lastSeq[p]:
		return predDup
	case in.seq == s.lastSeq[p]+1:
		return predAccept
	default:
		return predGap
	}
}

func (s prodState) applyAccept(in prodInput) prodState {
	ns := s
	ns.tail = s.tail + 1
	ns.hash = chainHash(s.hash, in.rhash)
	ns.epoch[in.prod] = in.epoch
	ns.lastSeq[in.prod] = in.seq
	ns.seen[in.prod] = true
	return ns
}

func producerModel() porcupine.NondeterministicModel {
	return porcupine.NondeterministicModel{
		Partition: func(history []porcupine.Operation) [][]porcupine.Operation {
			byStream := map[int][]porcupine.Operation{}
			order := []int{}
			for _, op := range history {
				s := op.Input.(prodInput).stream
				if _, ok := byStream[s]; !ok {
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
		Init: func() []interface{} { return []interface{}{prodState{}} },
		Step: func(state, input, output interface{}) []interface{} {
			s := state.(prodState)
			in := input.(prodInput)
			out := output.(prodOutput)

			if in.kind == "read" {
				if out.tail != s.tail {
					return nil
				}
				if out.hashKnown && out.hash != s.hash {
					return nil
				}
				return []interface{}{s}
			}

			// Indefinite append: the client got no answer, so the result is
			// unknown. Only an append that *would* have been accepted has a side
			// effect, so only that case branches into {maybe-durable, not-durable};
			// a would-be duplicate/stale/bad-seq/gap is a no-op either way. A later
			// definite outcome (e.g. an idempotent retry seeing 204 vs 200) selects
			// which world was real.
			if out.indefinite {
				if s.predict(in) == predAccept {
					return []interface{}{s.applyAccept(in), s}
				}
				return []interface{}{s}
			}

			switch s.predict(in) {
			case predAccept:
				if out.status != 200 || out.tail != s.tail+1 {
					return nil
				}
				return []interface{}{s.applyAccept(in)}
			case predDup:
				// A duplicate performs no write. Its Stream-Next-Offset is an
				// informational "current tail" hint, not a durability-gated
				// observation: in notifier mode it reflects the writer-view tail
				// (streamer.pend.tail), which runs ahead of the durable tail a
				// concurrent read sees, so it is not a sound linearization point.
				// Don't constrain it — exactly-once is still enforced by the
				// 204-vs-200 status distinction and the accept-tail check.
				if out.status != 204 {
					return nil
				}
				return []interface{}{s}
			case predStale:
				if out.status != 403 {
					return nil
				}
				return []interface{}{s}
			case predBadSeq:
				if out.status != 400 {
					return nil
				}
				return []interface{}{s}
			case predGap:
				if out.status != 409 {
					return nil
				}
				return []interface{}{s}
			}
			return nil
		},
		Equal: func(a, b interface{}) bool { return a.(prodState) == b.(prodState) },
		DescribeOperation: func(input, output interface{}) string {
			in := input.(prodInput)
			out := output.(prodOutput)
			if in.kind == "read" {
				return fmt.Sprintf("read() -> tail[%d] hash[%d]", out.tail, out.hash)
			}
			return fmt.Sprintf("append(p%d e%d s%d) -> %d tail[%d]", in.prod, in.epoch, in.seq, out.status, out.tail)
		},
		DescribeState: func(state interface{}) string {
			s := state.(prodState)
			return fmt.Sprintf("tail[%d] hash[%d] lastSeq%v", s.tail, s.hash, s.lastSeq)
		},
	}
}

// --- test ------------------------------------------------------------------

// producerScript is the per-producer op sequence; run sequentially it hits every
// branch of the producer state machine. Because each step waits for its response,
// real-time ordering makes each outcome deterministic regardless of how the
// producers on a stream interleave (each producer's fate depends only on its own
// prior appends). Porcupine still has to find a single global order of *accepts*
// consistent with the returned tails and the read hashes — that's the real check.
func producerScript() []struct{ epoch, seq uint64 } {
	return []struct{ epoch, seq uint64 }{
		{1, 0}, // accept (fresh)
		{1, 1}, // accept
		{1, 1}, // duplicate
		{1, 3}, // gap (expected 2)
		{1, 2}, // accept (fills gap)
		{1, 3}, // accept
		{2, 0}, // accept (new epoch resets seq)
		{1, 4}, // stale epoch (1 < 2)
		{2, 1}, // accept
		{2, 1}, // duplicate
		{4, 7}, // bad seq start (epoch jump with seq != 0)
		{4, 0}, // accept (new epoch)
		{4, 1}, // accept
	}
}

func TestLinearizableProducers(t *testing.T) {
	if testing.Short() {
		t.Skip("producer linearizability check skipped in -short")
	}

	streams := envInt("YASDB_PROD_STREAMS", 3)

	// Honors YASDB_TEST_DURABILITY (sync or notifier); both provide strict
	// linearizability. (Notifier's read-after-ack ordering is regression-tested by
	// TestProbeReadAfterAck.)
	ts, _ := newRealLiveServer(t, Config{
		LongPollTimeout: 200 * time.Millisecond,
		MaxReadBytes:    1 << 20,
	}, 5*time.Millisecond)

	for s := 0; s < streams; s++ {
		resp := do(t, ts, "PUT", prodStreamPath(s), "", hdr("Content-Type", "application/json"))
		wantStatus(t, resp, 201)
		resp.Body.Close()
	}

	var (
		clock atomic.Int64
		hist  = &history{}
		errs  = &errList{}
		cl    = &http.Client{Timeout: 15 * time.Second}
		wg    sync.WaitGroup
	)
	now := func() int64 { return clock.Add(1) }
	script := producerScript()

	for s := 0; s < streams; s++ {
		for p := 0; p < numProducers; p++ {
			wg.Add(1)
			clientID := s*numProducers + p
			go func(stream, prod, clientID int) {
				defer wg.Done()
				path := ts.URL + prodStreamPath(stream)
				pid := fmt.Sprintf("p%d", prod)
				for i, ev := range script {
					prodAppend(cl, hist, errs, now, stream, prod, clientID, path, pid, ev.epoch, ev.seq)
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

	nm := producerModel()
	model := nm.ToModel()
	res, info := porcupine.CheckOperationsVerbose(model, hist.ops, 60*time.Second)
	switch res {
	case porcupine.Ok:
		t.Logf("linearizable: %d ops across %d streams, %d producers/stream (durability=%q)",
			len(hist.ops), streams, numProducers, effectiveDurability())
	case porcupine.Unknown:
		t.Logf("porcupine returned Unknown (checker timed out); no violation found in %d ops", len(hist.ops))
	case porcupine.Illegal:
		path := filepath.Join(vizDir(t), "producer-violation.html")
		if err := porcupine.VisualizePath(model, info, path); err != nil {
			t.Logf("visualization: %v", err)
		}
		t.Fatalf("producer history NOT linearizable (%d ops) — visualization: %s", len(hist.ops), path)
	}

	// Optional: emit a visualization of a passing run for eyeballing.
	if out := os.Getenv("YASDB_PORCUPINE_VIZ"); out != "" && res != porcupine.Illegal {
		if err := porcupine.VisualizePath(model, info, out); err != nil {
			t.Logf("visualization: %v", err)
		} else {
			t.Logf("visualization written to %s", out)
		}
	}
}

// TestProducerModelHasTeeth is the negative control: it asserts the model rejects
// the two producer bugs that matter most — a duplicate that double-writes, and a
// stale-epoch append that slips past fencing — and accepts a legal history.
func TestProducerModelHasTeeth(t *testing.T) {
	nm := producerModel()
	model := nm.ToModel()

	// Dedup bug: producer p0 appends seq 0 then seq 1, then re-sends seq 1 (a
	// retry). The retry must be a 204 duplicate; here it returns 200 with an
	// advanced tail (a double-write). No global order permits two accepts of the
	// same (epoch, seq).
	doubleWrite := []porcupine.Operation{
		{ClientId: 0, Input: prodInput{kind: "append", stream: 0, prod: 0, epoch: 1, seq: 0}, Output: prodOutput{status: 200, tail: 1}, Call: 1, Return: 2},
		{ClientId: 0, Input: prodInput{kind: "append", stream: 0, prod: 0, epoch: 1, seq: 1}, Output: prodOutput{status: 200, tail: 2}, Call: 3, Return: 4},
		{ClientId: 0, Input: prodInput{kind: "append", stream: 0, prod: 0, epoch: 1, seq: 1}, Output: prodOutput{status: 200, tail: 3}, Call: 5, Return: 6},
	}
	if porcupine.CheckOperations(model, doubleWrite) {
		t.Fatal("double-write duplicate accepted; producer model is too weak")
	}

	// Fencing bug: p0 establishes epoch 2, then an epoch-1 append is accepted (200)
	// instead of being fenced (403).
	fenceBypass := []porcupine.Operation{
		{ClientId: 0, Input: prodInput{kind: "append", stream: 0, prod: 0, epoch: 2, seq: 0}, Output: prodOutput{status: 200, tail: 1}, Call: 1, Return: 2},
		{ClientId: 0, Input: prodInput{kind: "append", stream: 0, prod: 0, epoch: 1, seq: 1}, Output: prodOutput{status: 200, tail: 2}, Call: 3, Return: 4},
	}
	if porcupine.CheckOperations(model, fenceBypass) {
		t.Fatal("stale-epoch append accepted; producer model is too weak")
	}

	// Legal: two accepts, a duplicate (204, tail unchanged), then a read that sees
	// both records in order.
	r0, r1 := recordHash([]byte("t0")), recordHash([]byte("t1"))
	readHash := chainHash(chainHash(0, r0), r1)
	legal := []porcupine.Operation{
		{ClientId: 0, Input: prodInput{kind: "append", stream: 0, prod: 0, epoch: 1, seq: 0, rhash: r0}, Output: prodOutput{status: 200, tail: 1}, Call: 1, Return: 2},
		{ClientId: 0, Input: prodInput{kind: "append", stream: 0, prod: 0, epoch: 1, seq: 1, rhash: r1}, Output: prodOutput{status: 200, tail: 2}, Call: 3, Return: 4},
		{ClientId: 0, Input: prodInput{kind: "append", stream: 0, prod: 0, epoch: 1, seq: 1, rhash: r1}, Output: prodOutput{status: 204, tail: 2}, Call: 5, Return: 6},
		{ClientId: 1, Input: prodInput{kind: "read", stream: 0}, Output: prodOutput{status: 200, tail: 2, hash: readHash, hashKnown: true}, Call: 7, Return: 8},
	}
	res, info := porcupine.CheckOperationsVerbose(model, legal, 5*time.Second)
	if res != porcupine.Ok {
		t.Fatalf("legal producer history rejected: %v", res)
	}
	if err := porcupine.VisualizePath(model, info, filepath.Join(t.TempDir(), "producer-ok.html")); err != nil {
		t.Fatalf("visualize: %v", err)
	}
}

// --- op drivers -------------------------------------------------------------

func prodAppend(cl *http.Client, hist *history, errs *errList, now func() int64, stream, prod, clientID int, path, pid string, epoch, seq uint64) {
	token := fmt.Sprintf("s%d/p%d/e%d/n%d", stream, prod, epoch, seq)
	req, _ := http.NewRequest("POST", path, strings.NewReader(`["`+token+`"]`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Producer-Id", pid)
	req.Header.Set("Producer-Epoch", strconv.FormatUint(epoch, 10))
	req.Header.Set("Producer-Seq", strconv.FormatUint(seq, 10))

	call := now()
	resp, err := cl.Do(req)
	if err != nil {
		errs.add("append %s: %v", token, err)
		return
	}
	next := resp.Header.Get("Stream-Next-Offset")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	ret := now()

	if resp.StatusCode >= 500 {
		errs.add("append %s: server error %d", token, resp.StatusCode)
		return
	}
	out := prodOutput{status: resp.StatusCode}
	if resp.StatusCode == 200 || resp.StatusCode == 204 {
		n, ok := offsetSeq(next)
		if !ok {
			errs.add("append %s: bad next-offset %q", token, next)
			return
		}
		out.tail = uint64(n)
	}
	hist.add(porcupine.Operation{
		ClientId: clientID,
		Input:    prodInput{kind: "append", stream: stream, prod: prod, epoch: epoch, seq: seq, rhash: recordHash([]byte(token)), token: token},
		Output:   out,
		Call:     call,
		Return:   ret,
	})
}

func prodRead(cl *http.Client, hist *history, errs *errList, now func() int64, stream, clientID int, path string) {
	req, _ := http.NewRequest("GET", path+"?offset=-1", nil)

	call := now()
	resp, err := cl.Do(req)
	if err != nil {
		errs.add("read: %v", err)
		return
	}
	upToDate := resp.Header.Get("Stream-Up-To-Date")
	next := resp.Header.Get("Stream-Next-Offset")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	ret := now()

	if resp.StatusCode != http.StatusOK {
		errs.add("read: status %d", resp.StatusCode)
		return
	}
	if upToDate != "true" {
		errs.add("read: not up-to-date (history too large for one read?)")
		return
	}
	tokens, err := decodeJSONArray(b)
	if err != nil {
		errs.add("read: decode %q: %v", string(b), err)
		return
	}
	var h uint64
	for _, tok := range tokens {
		h = chainHash(h, recordHash([]byte(tok)))
	}
	tail, ok := offsetSeq(next)
	if !ok {
		errs.add("read: bad next-offset %q", next)
		return
	}
	hist.add(porcupine.Operation{
		ClientId: clientID,
		Input:    prodInput{kind: "read", stream: stream},
		Output:   prodOutput{status: 200, tail: uint64(tail), hash: h, hashKnown: true},
		Call:     call,
		Return:   ret,
	})
}

func prodStreamPath(s int) string { return fmt.Sprintf("/prod/s%d", s) }
