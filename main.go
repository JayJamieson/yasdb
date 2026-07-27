// Command yasdb runs a Durable Streams server backed by SlateDB.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/JayJamieson/yasdb/internal/ds"
)

func main() {
	addr := flag.String("addr", ":4437", "listen address")
	storeURL := flag.String("store", "", "object store URL (e.g. memory:///, s3://bucket/prefix); defaults to a local file store under -data")
	dataDir := flag.String("data", "./yasdb-data", "local data directory used when -store is empty")
	dbPath := flag.String("db", "yasdb", "database path/prefix within the object store")
	flush := flag.Duration("flush", 10*time.Millisecond, "SlateDB WAL flush interval (durable-append latency floor); 0 = engine default (100ms)")
	durability := flag.String("durability", "sync", "append durability model: sync (block each write on durability) or notifier (non-blocking write + durable-seq watcher; pipelines appends)")
	pollInterval := flag.Duration("notifier-poll", time.Millisecond, "durable-seq poll cadence in -durability notifier mode")
	liveCoalesce := flag.Duration("live-coalesce", 0, "coalesce live-reader (SSE/long-poll) wakeups within this window to tame fan-out under high commit-rate × many readers; 0 = wake on every commit")
	noLiveCache := flag.Bool("no-live-cache", false, "disable the in-memory recent-records cache that lets caught-up live readers avoid a store scan (escape hatch; correctness is unchanged)")
	longPollTimeout := flag.Duration("longpoll-timeout", 0, "how long a live=long-poll read blocks waiting for data before returning 204; 0 = default (25s)")
	liveCacheBytes := flag.Int("live-cache-bytes", 0, "per-stream cap on the in-memory recent-records cache (bytes); 0 = default (256 KiB). Lower to cut memory on many-hot-stream deployments")
	flag.Parse()

	url, dbp, err := ds.ResolveStore(*storeURL, *dataDir, *dbPath)
	if err != nil {
		log.Fatalf("%v", err)
	}

	store, err := ds.OpenStore(dbp, url, *flush)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	srv, err := ds.NewServer(store, ds.Config{
		Durability:           *durability,
		NotifierPollInterval: *pollInterval,
		LiveCoalesceWindow:   *liveCoalesce,
		DisableLiveCache:     *noLiveCache,
		LongPollTimeout:      *longPollTimeout,
		LiveCacheMaxBytes:    *liveCacheBytes,
	})
	if err != nil {
		log.Fatalf("start server: %v", err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Liveness/readiness probe (e.g. provisor --health-path /__health). Kept
		// out of the stream keyspace so it never collides with a stream path.
		if r.URL.Path == "/__health" {
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				w.Write([]byte("ok\n"))
			}
			return
		}
		if r.URL.Path == "/__ui" || strings.HasPrefix(r.URL.Path, "/__ui/") {
			serveUI(w, r)
			return
		}
		srv.ServeHTTP(w, r)
	})

	httpSrv := &http.Server{Addr: *addr, Handler: handler}

	uiHost := *addr
	if strings.HasPrefix(uiHost, ":") {
		uiHost = "localhost" + uiHost
	}
	go func() {
		log.Printf("yasdb listening on %s (store=%s, durability=%s, ui=http://%s/__ui/)", *addr, url, *durability, uiHost)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	if err := srv.Close(); err != nil {
		log.Printf("close: %v", err)
	}
}
