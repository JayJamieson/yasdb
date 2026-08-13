package ds

import (
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func benchServer(b *testing.B, storeURL string, flush time.Duration) *Server {
	b.Helper()
	if storeURL == "" {
		storeURL = "memory:///"
	}
	store, err := OpenStore(uniqueDBPath("bench"), storeURL, StoreTuning{FlushInterval: flush})
	if err != nil {
		b.Fatal(err)
	}
	srv, err := NewServer(store, Config{Durability: testDurability(), NotifierPollInterval: time.Millisecond})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = srv.Close() })
	return srv
}

func benchFileServer(b *testing.B, flush time.Duration) *Server {
	b.Helper()
	dir, err := os.MkdirTemp("", "yasdb-bench-*")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = os.RemoveAll(dir) })
	dbp := strings.TrimPrefix(dir, "/") + "/db"
	store, err := OpenStore(dbp, "file:///", StoreTuning{FlushInterval: flush})
	if err != nil {
		b.Fatal(err)
	}
	srv, err := NewServer(store, Config{Durability: testDurability(), NotifierPollInterval: time.Millisecond})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = srv.Close() })
	return srv
}

func mustCreate(b *testing.B, srv *Server, path, ct string) {
	b.Helper()
	req := httptest.NewRequest("PUT", path, nil)
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 201 {
		b.Fatalf("create %s: %d", path, rr.Code)
	}
}

var benchRec = [][]byte{[]byte("hello world")}

func appendOnce(b *testing.B, srv *Server, path string) {
	resp, ok := srv.submitAppend(path, appendReq{records: benchRec, hasBody: true, contentType: "text/plain"}, nil)
	if !ok || resp.status != 204 {
		b.Errorf("append %s: ok=%v status=%d", path, ok, resp.status)
	}
}

// benchSerialSrv: one durable append at a time against an already-built
// server — latency floor is whatever the backend's durable-write path costs
// (SlateDB: bound by flush interval; kv: bound by its 2PC commit cost).
func benchSerialSrv(b *testing.B, srv *Server, path string) {
	mustCreate(b, srv, path, "text/plain")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		appendOnce(b, srv, path)
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "appends/s")
}

// benchSerial: one durable append at a time — latency floor = flush interval.
func benchSerial(b *testing.B, flush time.Duration) {
	benchSerialSrv(b, benchServer(b, "", flush), "/s")
}

// benchBurst: `conc` concurrent producers hammer ONE stream. The streamer folds
// queued appends into group commits, so throughput exceeds 1/flush.
func benchBurst(b *testing.B, srv *Server, path string, conc int) {
	var idx atomic.Int64
	target := int64(b.N)
	b.ReportAllocs()
	b.ResetTimer()
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx.Add(1) <= target {
				appendOnce(b, srv, path)
			}
		}()
	}
	wg.Wait()
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "appends/s")
}

// Serial durable-append latency (dominated by the flush interval).
func BenchmarkAppendSerialFlush10ms(b *testing.B) { benchSerial(b, 10*time.Millisecond) }

// Group-commit throughput on one stream. 128 shows sync's maxBurst/flush ceiling;
// 1024 shows notifier scaling past it (YASDB_TEST_DURABILITY=notifier).
func BenchmarkAppendBurstFlush10ms(b *testing.B) {
	srv := benchServer(b, "", 10*time.Millisecond)
	mustCreate(b, srv, "/s", "text/plain")
	benchBurst(b, srv, "/s", 128)
}

func BenchmarkAppendBurst1024Flush10ms(b *testing.B) {
	srv := benchServer(b, "", 10*time.Millisecond)
	mustCreate(b, srv, "/s", "text/plain")
	benchBurst(b, srv, "/s", 1024)
}

// On-disk file store (real fsync durability), same group-commit path.
func BenchmarkAppendBurstFileStore10ms(b *testing.B) {
	srv := benchFileServer(b, 10*time.Millisecond)
	mustCreate(b, srv, "/s", "text/plain")
	benchBurst(b, srv, "/s", 128)
}

// benchNotifierServer forces notifier durability regardless of
// YASDB_TEST_DURABILITY, since sharedCommitter (the thing under test below)
// only activates in that mode (see newCommitter).
func benchNotifierServer(b *testing.B, flush time.Duration) *Server {
	b.Helper()
	store, err := OpenStore(uniqueDBPath("bench"), "memory:///", StoreTuning{FlushInterval: flush})
	if err != nil {
		b.Fatal(err)
	}
	srv, err := NewServer(store, Config{Durability: "notifier", NotifierPollInterval: time.Millisecond})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = srv.Close() })
	return srv
}

// benchManyStreams exercises CROSS-stream group commit: numStreams
// concurrently-hot streams, each with producersPerStream producers, under
// notifier durability. Every other burst benchmark in this file uses ONE
// stream — none of them exercise sharedCommitter's actual reason for
// existing: coalescing bursts ACROSS many different hot streams into
// fewer physical CommitAsync calls. This benchmark was written to A/B a
// worker-pool-per-stream-hash-bucket design against the current single
// shared queue; the worker pool showed a real edge here (in-memory store,
// 2 vCPUs) but none against real hardware (see RFC 0001) — kept as a
// permanent regression guard for this dimension, not just a one-off A/B
// tool.
//
// producersPerStream=1 is the purest signal: with only one producer per
// stream, a stream's own drainBurst can never coalesce anything (there is
// never more than one queued append per stream at a time), so ANY
// throughput above the serial baseline must come from sharedCommitter's
// cross-stream fold, not per-stream batching.
func benchManyStreams(b *testing.B, numStreams, producersPerStream int) {
	srv := benchNotifierServer(b, 10*time.Millisecond)
	paths := make([]string, numStreams)
	for i := range paths {
		paths[i] = fmt.Sprintf("/s%d", i)
		mustCreate(b, srv, paths[i], "text/plain")
		// mustCreate (PUT) only persists metadata; it does not spawn a
		// resident streamer (see createStream/handlePut — that's lazy, via
		// getOrSpawn on first touch). Without this warm-up append, all
		// numStreams streamers would cold-spawn simultaneously at
		// b.ResetTimer(), clustering a one-time registry-spawn-lock storm
		// into the very start of the timed region and dominating a mutex
		// profile with a startup cost, not steady-state contention.
		appendOnce(b, srv, paths[i])
	}
	var idx atomic.Int64
	target := int64(b.N)
	b.ReportAllocs()
	b.ResetTimer()
	var wg sync.WaitGroup
	for s := 0; s < numStreams; s++ {
		path := paths[s]
		for p := 0; p < producersPerStream; p++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for idx.Add(1) <= target {
					appendOnce(b, srv, path)
				}
			}()
		}
	}
	wg.Wait()
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "appends/s")
}

func BenchmarkAppendBurstManyStreams1024x1(b *testing.B) { benchManyStreams(b, 1024, 1) }
func BenchmarkAppendBurstManyStreams256x4(b *testing.B)  { benchManyStreams(b, 256, 4) }
func BenchmarkAppendBurstManyStreams64x1(b *testing.B)   { benchManyStreams(b, 64, 1) }

func BenchmarkReadCatchup(b *testing.B) {
	srv := benchServer(b, "", 5*time.Millisecond)
	mustCreate(b, srv, "/r", "text/plain")
	for i := 0; i < 100; i++ {
		appendOnce(b, srv, "/r")
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := httptest.NewRequest("GET", "/r?offset=-1", nil)
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			if rr.Code != 200 {
				b.Errorf("read: %d", rr.Code)
				return
			}
		}
	})
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "reads/s")
}

// BenchmarkAppendBurstS3 runs against an object store (e.g. MinIO); skipped unless
// YASDB_BENCH_STORE (+ AWS_* env) is set. YASDB_BENCH_FLUSH overrides the flush.
func BenchmarkAppendBurstS3(b *testing.B) {
	url := os.Getenv("YASDB_BENCH_STORE")
	if url == "" {
		b.Skip("set YASDB_BENCH_STORE=s3://bucket (+ AWS_* env) to run")
	}
	flush := 10 * time.Millisecond
	if v := os.Getenv("YASDB_BENCH_FLUSH"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			flush = d
		}
	}
	srv := benchServer(b, url, flush)
	mustCreate(b, srv, "/s", "text/plain")
	benchBurst(b, srv, "/s", 1024)
}
