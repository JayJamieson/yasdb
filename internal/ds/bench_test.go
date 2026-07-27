package ds

import (
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
	store, err := OpenStore(uniqueDBPath("bench"), storeURL, flush)
	if err != nil {
		b.Fatal(err)
	}
	srv, err := NewServer(store, Config{Durability: testDurability(), NotifierPollInterval: time.Millisecond})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { srv.Close() })
	return srv
}

func benchFileServer(b *testing.B, flush time.Duration) *Server {
	b.Helper()
	dir, err := os.MkdirTemp("", "yasdb-bench-*")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { os.RemoveAll(dir) })
	dbp := strings.TrimPrefix(dir, "/") + "/db"
	store, err := OpenStore(dbp, "file:///", flush)
	if err != nil {
		b.Fatal(err)
	}
	srv, err := NewServer(store, Config{Durability: testDurability(), NotifierPollInterval: time.Millisecond})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { srv.Close() })
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
	resp, ok := srv.submitAppend(path, appendReq{records: benchRec, hasBody: true, contentType: "text/plain"})
	if !ok || resp.status != 204 {
		b.Errorf("append %s: ok=%v status=%d", path, ok, resp.status)
	}
}

// benchSerial: one durable append at a time — latency floor = flush interval.
func benchSerial(b *testing.B, flush time.Duration) {
	srv := benchServer(b, "", flush)
	mustCreate(b, srv, "/s", "text/plain")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		appendOnce(b, srv, "/s")
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "appends/s")
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
