package ds

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkLiveFanout measures per-commit fan-out cost to a fixed reader pool
// under v1 (immediate) and v2 (coalesced) wake modes.
func BenchmarkLiveFanout(b *testing.B) {
	for _, readers := range []int{100, 500} {
		for _, mode := range []struct {
			name   string
			window time.Duration
		}{{"v1", 0}, {"v2_10ms", 10 * time.Millisecond}, {"v2_50ms", 50 * time.Millisecond}} {
			b.Run(fmt.Sprintf("readers=%d/%s", readers, mode.name), func(b *testing.B) {
				benchFanout(b, readers, 1, mode.window, false)
			})
		}
	}
}

// BenchmarkLiveFanoutHub isolates the in-memory record cache: same coalesce
// window, cache off vs on. Cache-on should serve ~every live read from RAM.
func BenchmarkLiveFanoutHub(b *testing.B) {
	for _, readers := range []int{100, 500} {
		for _, hub := range []struct {
			name         string
			disableCache bool
		}{{"hub_off", true}, {"hub_on", false}} {
			b.Run(fmt.Sprintf("readers=%d/%s", readers, hub.name), func(b *testing.B) {
				benchFanout(b, readers, 1, 10*time.Millisecond, hub.disableCache)
			})
		}
	}
}

func benchFanout(b *testing.B, readers, writers int, window time.Duration, disableCache bool) {
	if writers < 1 {
		writers = 1
	}
	ts, srv := newRealLiveServer(b, Config{LiveCoalesceWindow: window, DisableLiveCache: disableCache}, time.Millisecond)
	const recW = 10
	path := "/bench"
	putReq, _ := http.NewRequest("PUT", ts.URL+path, nil)
	putReq.Header.Set("Content-Type", "text/plain")
	if resp, err := http.DefaultClient.Do(putReq); err == nil {
		resp.Body.Close()
	}
	rec := func(i int) string { return fmt.Sprintf("%0*d", recW, i) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sseURL := ts.URL + path + "?offset=-1&live=sse"
	var received atomic.Int64 // total records delivered across all readers
	var ready sync.WaitGroup
	ready.Add(readers)
	client := &http.Client{}
	for r := 0; r < readers; r++ {
		go func() {
			req, _ := http.NewRequestWithContext(ctx, "GET", sseURL, nil)
			resp, err := client.Do(req)
			if err != nil {
				ready.Done()
				return
			}
			defer resp.Body.Close()
			ready.Done()
			sc := bufio.NewScanner(resp.Body)
			sc.Buffer(make([]byte, 1<<16), 1<<20)
			isData := false
			for sc.Scan() {
				line := sc.Text()
				switch {
				case line == "event: data":
					isData = true
				case strings.HasPrefix(line, "event:"):
					isData = false
				case strings.HasPrefix(line, "data:") && isData:
					received.Add(int64(len(strings.TrimPrefix(line, "data:")) / recW))
				}
			}
		}()
	}
	ready.Wait()

	st, _, _ := srv.getOrSpawn(path)
	wake0 := st.wakeCount.Load()
	b.ResetTimer()
	// `writers` goroutines share the b.N appends to the one stream. With >1 they
	// contend on the streamer's reqs channel and get folded by group commit.
	var wi atomic.Int64
	var wwg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wwg.Add(1)
		go func() {
			defer wwg.Done()
			for {
				i := wi.Add(1) - 1
				if i >= int64(b.N) {
					return
				}
				srv.submitAppend(path, appendReq{records: [][]byte{[]byte(rec(int(i)))}, hasBody: true, contentType: "text/plain"}, nil)
			}
		}()
	}
	wwg.Wait()
	// Drain: wait until every reader has caught up to the tail.
	target := int64(readers) * int64(b.N)
	deadline := time.Now().Add(20 * time.Second)
	for received.Load() < target && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	b.StopTimer()

	wakes := st.wakeCount.Load() - wake0
	var hits int64
	if st.cache != nil {
		hits = st.cache.hits.Load()
	}
	b.ReportMetric(float64(received.Load())/b.Elapsed().Seconds(), "deliver/s")
	b.ReportMetric(float64(wakes), "wakes")
	b.ReportMetric(float64(wakes)/float64(max(int64(b.N), 1)), "wakes/commit")
	b.ReportMetric(float64(hits)/float64(max(int64(b.N), 1)), "cachehits/commit")
}
