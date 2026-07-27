// Command maelstrom-adapter bridges a Jepsen Maelstrom "kafka" workload to a
// running yasdb server, so Maelstrom's log-consistency checker can validate
// yasdb's append/read path under concurrency.
//
// Maelstrom speaks newline-delimited JSON on stdin/stdout; this adapter
// translates each RPC to yasdb HTTP against $YASDB_URL:
//
//	send {key,msg}                 -> POST /mlst/<key>   (JSON append)  -> offset
//	poll {offsets}                 -> GET  /mlst/<key>?offset=<seq>     -> [[offset,msg]]
//	commit_offsets / list_...      -> in-process committed-offset store
//
// A Kafka offset is the message's yasdb sequence number: appending one JSON
// message advances the tail by one, so the message's offset = nextSeq-1, and a
// poll from offset N reads the JSON array of messages at seqs N, N+1, ….
//
// Run with a single Maelstrom node (--node-count 1) so committed offsets share
// one process; concurrency comes from --concurrency/--rate, all hitting the one
// shared yasdb server. This is a pure-Go binary (no cgo / SlateDB needed).
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	base   = envOr("YASDB_URL", "http://127.0.0.1:4437")
	client = &http.Client{Timeout: 30 * time.Second}

	// readMode selects how polls read: "catchup" (default, GET ?offset=) or
	// "long-poll" (GET ?offset=&live=long-poll), which drives yasdb's live-read +
	// record-cache path so Maelstrom validates it under randomized load. Run the
	// server with a short -longpoll-timeout so caught-up polls return 204 quickly.
	readMode = envOr("YASDB_MLST_READ", "catchup")

	outMu  sync.Mutex // serialises stdout writes
	nextID atomic.Int64

	commitMu sync.Mutex
	commits  = map[string]int64{} // committed offset per key (single-node)
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

var (
	childMu   sync.Mutex
	child     *exec.Cmd
	yasdbBin  string
	yasdbArgs []string
)

// superviseYasdb starts the yasdb binary as a child bound to a persistent store
// on a fixed port, waits for health, and points `base` at it. The child dies
// with us via PDEATHSIG. When YASDB_CHAOS_MS is set, a background loop SIGKILLs
// and restarts yasdb on that cadence — real crashes on the same persistent store
// — so Maelstrom's log-consistency checker verifies that acked writes survive
// crashes and recovery. yasdb logs go to our stderr (stdout is the Maelstrom
// protocol channel).
func superviseYasdb(bin string) {
	runtime.LockOSThread() // keep PDEATHSIG bound to a live thread
	yasdbBin = bin
	yasdbArgs = []string{
		"-addr", envOr("YASDB_ADDR", "127.0.0.1:4700"),
		"-data", envOr("YASDB_DATA", "/tmp/yasdb-nemesis"),
		"-flush", envOr("YASDB_FLUSH", "5ms"),
		"-durability", envOr("YASDB_DUR", "sync"),
		"-notifier-poll", "1ms",
	}
	base = "http://" + envOr("YASDB_ADDR", "127.0.0.1:4700")

	startYasdb()
	if !waitHealth(30 * time.Second) {
		fmt.Fprintln(os.Stderr, "supervise: yasdb did not become healthy")
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "supervise: yasdb ready")

	if ms, _ := strconv.Atoi(os.Getenv("YASDB_CHAOS_MS")); ms > 0 {
		go chaosLoop(time.Duration(ms) * time.Millisecond)
	}
}

func startYasdb() {
	cmd := exec.Command(yasdbBin, yasdbArgs...)
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "supervise: start yasdb: %v\n", err)
		os.Exit(1)
	}
	childMu.Lock()
	child = cmd
	childMu.Unlock()
}

// chaosLoop repeatedly crashes (SIGKILL) yasdb and restarts it, forcing recovery
// from the persistent store while the workload runs.
func chaosLoop(interval time.Duration) {
	for {
		time.Sleep(interval + time.Duration(nextID.Load()%1000)*time.Millisecond)
		childMu.Lock()
		c := child
		childMu.Unlock()
		fmt.Fprintln(os.Stderr, "chaos: SIGKILL yasdb")
		_ = c.Process.Kill() // hard crash — no graceful shutdown
		_ = c.Wait()
		time.Sleep(300 * time.Millisecond) // brief downtime
		startYasdb()
		waitHealth(30 * time.Second) // recovery (WAL replay)
		fmt.Fprintln(os.Stderr, "chaos: yasdb recovered")
	}
}

func waitHealth(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if resp, err := client.Get(base + "/__health"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

type message struct {
	Src  string          `json:"src"`
	Dest string          `json:"dest"`
	Body json.RawMessage `json:"body"`
}

type reqBody struct {
	Type    string           `json:"type"`
	MsgID   int64            `json:"msg_id"`
	NodeID  string           `json:"node_id"`
	Key     string           `json:"key"`
	Msg     json.RawMessage  `json:"msg"`
	Offsets map[string]int64 `json:"offsets"`
	Keys    []string         `json:"keys"`
}

func main() {
	// Fault-injection mode: when YASDB_BIN is set, supervise yasdb as a child so
	// Maelstrom's :kill / :pause nemesis crashes/pauses the actual server (not
	// just this bridge), exercising crash recovery + durability. The child dies
	// with us via PDEATHSIG, and on restart recovers from its persistent store.
	if bin := os.Getenv("YASDB_BIN"); bin != "" {
		superviseYasdb(bin)
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var wg sync.WaitGroup
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		wg.Add(1)
		go func() {
			defer wg.Done()
			handle(line)
		}()
	}
	wg.Wait()
}

func handle(line []byte) {
	var m message
	if err := json.Unmarshal(line, &m); err != nil {
		return
	}
	var b reqBody
	if err := json.Unmarshal(m.Body, &b); err != nil {
		return
	}
	switch b.Type {
	case "init":
		reply(m, map[string]any{"type": "init_ok"}, b.MsgID)
	case "send":
		off, err := doSend(b.Key, b.Msg)
		if err != nil {
			replyErr(m, b.MsgID, err)
			return
		}
		reply(m, map[string]any{"type": "send_ok", "offset": off}, b.MsgID)
	case "poll":
		msgs, err := doPoll(b.Offsets)
		if err != nil {
			replyErr(m, b.MsgID, err)
			return
		}
		reply(m, map[string]any{"type": "poll_ok", "msgs": msgs}, b.MsgID)
	case "commit_offsets":
		commitMu.Lock()
		for k, off := range b.Offsets {
			if off > commits[k] {
				commits[k] = off
			}
		}
		commitMu.Unlock()
		reply(m, map[string]any{"type": "commit_offsets_ok"}, b.MsgID)
	case "list_committed_offsets":
		out := map[string]int64{}
		commitMu.Lock()
		for _, k := range b.Keys {
			if v, ok := commits[k]; ok {
				out[k] = v
			}
		}
		commitMu.Unlock()
		reply(m, map[string]any{"type": "list_committed_offsets_ok", "offsets": out}, b.MsgID)
	}
}

// doSend appends one JSON message and returns its Kafka offset (its yasdb seq).
func doSend(key string, msg json.RawMessage) (int64, error) {
	streamURL := base + "/mlst/" + url.PathEscape(key)
	nextOff, status, err := postJSON(streamURL, msg)
	if err != nil {
		return 0, err
	}
	if status == http.StatusNotFound {
		// create the JSON stream, then retry the append
		if err := putStream(streamURL); err != nil {
			return 0, err
		}
		nextOff, status, err = postJSON(streamURL, msg)
		if err != nil {
			return 0, err
		}
	}
	if status != http.StatusNoContent && status != http.StatusOK {
		return 0, fmt.Errorf("append status %d", status)
	}
	// Stream-Next-Offset is the tail AFTER this append; the message sits at
	// tail-1 (one JSON message per send).
	return int64(nextOff) - 1, nil
}

// doPoll reads each key from its requested offset and pairs messages with their
// offsets.
func doPoll(offsets map[string]int64) (map[string][][2]any, error) {
	out := map[string][][2]any{}
	for key, off := range offsets {
		streamURL := base + "/mlst/" + url.PathEscape(key) + "?offset=" + offsetToken(uint64(off))
		if readMode == "long-poll" {
			streamURL += "&live=long-poll"
		}
		body, status, err := getBody(streamURL)
		if err != nil {
			return nil, err
		}
		// 404: stream not created yet. 204: caught up (long-poll timed out with no
		// new data). Both mean "no messages for this key right now."
		if status == http.StatusNotFound || status == http.StatusNoContent {
			continue
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("poll status %d", status)
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(body, &arr); err != nil {
			return nil, fmt.Errorf("poll body %q: %w", body, err)
		}
		pairs := make([][2]any, len(arr))
		for j, raw := range arr {
			pairs[j] = [2]any{off + int64(j), raw}
		}
		if len(pairs) > 0 {
			out[key] = pairs
		}
	}
	return out, nil
}

// --- yasdb HTTP helpers ---

func putStream(streamURL string) error {
	req, _ := http.NewRequest("PUT", streamURL, nil)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("put status %d", resp.StatusCode)
	}
	return nil
}

func postJSON(streamURL string, msg json.RawMessage) (nextSeq uint64, status int, err error) {
	req, _ := http.NewRequest("POST", streamURL, bytes.NewReader(msg))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return 0, http.StatusNotFound, nil
	}
	seq, _ := parseOffset(resp.Header.Get("Stream-Next-Offset"))
	return seq, resp.StatusCode, nil
}

func getBody(streamURL string) ([]byte, int, error) {
	resp, err := client.Get(streamURL)
	if err != nil {
		return nil, 0, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return body, resp.StatusCode, nil
}

func offsetToken(seq uint64) string { return fmt.Sprintf("%016d_%016d", seq, 0) }

func parseOffset(tok string) (uint64, error) {
	i := strings.IndexByte(tok, '_')
	if i < 0 {
		return 0, fmt.Errorf("bad offset %q", tok)
	}
	return strconv.ParseUint(tok[:i], 10, 64)
}

// --- Maelstrom reply plumbing ---

func reply(in message, body map[string]any, inReplyTo int64) {
	body["in_reply_to"] = inReplyTo
	body["msg_id"] = nextID.Add(1)
	send(message{Src: in.Dest, Dest: in.Src, Body: mustJSON(body)})
}

func replyErr(in message, inReplyTo int64, err error) {
	// code 13 = crash (temporary) in the Maelstrom error taxonomy.
	reply(in, map[string]any{"type": "error", "code": 13, "text": err.Error()}, inReplyTo)
}

func send(m message) {
	line := mustJSON(m)
	outMu.Lock()
	os.Stdout.Write(line)
	os.Stdout.Write([]byte{'\n'})
	outMu.Unlock()
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal: %v\n", err)
		return []byte("{}")
	}
	return b
}
