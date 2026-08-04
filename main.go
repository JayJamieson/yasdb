// Command yasdb runs a Durable Streams server backed by SlateDB.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/JayJamieson/yasdb/internal/ds"
)

// version, commit, and date are set at build time via -ldflags (see
// .goreleaser.yaml). They stay at these defaults for `go run`/`go build`
// without ldflags, e.g. local development.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// defaultLiveCoalesceWindow bounds how often parked live readers (SSE/long-poll)
// wake for new commits. It folds a burst into one trailing wake, instead of
// waking every reader on every commit (the thundering-herd baseline). This is
// not a flag: BENCHMARKS.md's "Live fan-out" section found a small nonzero
// window is a strict win with no downside for low-rate streams (leading-edge:
// no added latency until commits arrive faster than this window). So it stays
// always on.
const defaultLiveCoalesceWindow = 5 * time.Millisecond

func main() {
	addr := flag.String("addr", ":4437", "listen address")
	storeURL := flag.String("store", "", "object store URL (e.g. memory:///, s3://bucket/prefix); defaults to a local file store under -data")
	dataDir := flag.String("data", "./yasdb-data", "local data directory used when -store is empty")
	dbPath := flag.String("db", "yasdb", "database path/prefix within the object store")
	flush := flag.Duration("flush", 10*time.Millisecond, "SlateDB WAL flush interval (durable-append latency floor); 0 = engine default (100ms)")
	durability := flag.String("durability", "notifier", "append durability model. notifier: non-blocking write plus a durable-seq watcher; pipelines appends; scales with concurrent writers (default). sync: blocks each write on durability; lower latency for a single low-concurrency writer, and what crash-recovery testing uses.")
	pollInterval := flag.Duration("notifier-poll", time.Millisecond, "durable-seq poll interval in -durability notifier mode")
	longPollTimeout := flag.Duration("longpoll-timeout", 0, "how long a live=long-poll read blocks waiting for data before it returns 204; 0 = default (25s)")
	metricsAddr := flag.String("metrics-addr", "", "listen address for a Prometheus /metrics endpoint that exposes native SlateDB engine metrics; empty = disabled. Serve this on a separate, non-public address (e.g. Fly's private-network-only metrics port, conventionally :9091), not on -addr.")
	slatedbLogLevel := flag.String("slatedb-log-level", "warn", "SlateDB native (Rust-side) log level written to stderr: off|error|warn|info|debug|trace. Use debug or trace to diagnose stalls; both are verbose (per-flush/compaction detail) and noisy for steady-state production.")
	compactorMaxJobs := flag.Int("compactor-max-jobs", 0, "max concurrent SlateDB compaction jobs; 0 = engine default (4)")
	compactorMaxSubcompactions := flag.Int("compactor-max-subcompactions", 0, "max parallel workers per SlateDB compaction job; 0 = engine default (4). Lower this if compaction starves foreground writes (see BENCHMARKS.md's \"Compaction stall under load\").")
	l0FlushParallelism := flag.Int("l0-flush-parallelism", 0, "max concurrent L0 SST flush uploads; 0 = engine default (4)")
	pwalShards := flag.Int("pwal-shards", 8, "shard count for -store pwal:///<dir> (experimental sharded-WAL backend; ignored for every other -store scheme)")
	pprofBlockRate := flag.Int("pprof-block-rate", 0, "runtime.SetBlockProfileRate in nanoseconds (1 = profile every blocking event); 0 = off. Mounts net/http/pprof on -metrics-addr when this or -pprof-mutex-fraction is set. Investigative use only; adds real overhead when nonzero.")
	pprofMutexFraction := flag.Int("pprof-mutex-fraction", 0, "runtime.SetMutexProfileFraction (1 = sample every mutex contention event); 0 = off. See -pprof-block-rate.")
	adminBulkProvision := flag.Bool("admin-bulk-provision", false, "mount POST /__admin/bulk-provision on -metrics-addr (private-network-only). Creates many empty streams in a few batched Commit calls, instead of one HTTP request per stream, for fast load-test setup. Off by default: it is a mutating, storage-filling operation.")
	committerWorkers := flag.Int("committer-workers", 0, "shared committer pool size in -durability notifier mode (see docs/rfcs/0001-cross-stream-group-commit.md); 0 = GOMAXPROCS")
	committerBatchMax := flag.Int("committer-batch-max", 0, "max number of different streams' bursts one committer folds into a single WriteBatch; 0 = engine default (512)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("yasdb %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	if err := ds.InitSlateDBLogging(*slatedbLogLevel); err != nil {
		log.Fatalf("%v", err)
	}

	var store ds.Storage
	var url string
	var err error
	if strings.HasPrefix(*storeURL, "pwal://") {
		// This is the experimental sharded-WAL backend (see
		// internal/ds/pwalstore.go). It bypasses SlateDB/ResolveStore
		// entirely. *storeURL is a plain local directory path after the
		// scheme, e.g. pwal:///var/lib/yasdb-pwal.
		url = *storeURL
		dir := strings.TrimPrefix(*storeURL, "pwal://")
		store, err = ds.OpenPWALStore(dir, *pwalShards)
		if err != nil {
			log.Fatalf("open pwal store: %v", err)
		}
	} else {
		var dbp string
		url, dbp, err = ds.ResolveStore(*storeURL, *dataDir, *dbPath)
		if err != nil {
			log.Fatalf("%v", err)
		}
		store, err = ds.OpenStore(dbp, url, ds.StoreTuning{
			FlushInterval:              *flush,
			CompactorMaxConcurrentJobs: *compactorMaxJobs,
			CompactorMaxSubcompactions: *compactorMaxSubcompactions,
			L0FlushParallelism:         *l0FlushParallelism,
		})
		if err != nil {
			log.Fatalf("open store: %v", err)
		}
	}
	srv, err := ds.NewServer(store, ds.Config{
		Durability:           *durability,
		NotifierPollInterval: *pollInterval,
		LiveCoalesceWindow:   defaultLiveCoalesceWindow,
		LongPollTimeout:      *longPollTimeout,
		CommitterWorkers:     *committerWorkers,
		CommitterBatchMax:    *committerBatchMax,
	})
	if err != nil {
		log.Fatalf("start server: %v", err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Liveness/readiness probe (see fly.toml's health check). Kept out of
		// the stream keyspace so it never collides with a stream path.
		if r.URL.Path == "/__health" {
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				w.Write([]byte("ok\n"))
			}
			return
		}
		if r.URL.Path == stateSnapshotPrefix || strings.HasPrefix(r.URL.Path, stateSnapshotPrefix+"/") {
			serveStateSnapshot(srv, w, r)
			return
		}
		srv.ServeHTTP(w, r)
	})

	httpSrv := &http.Server{Addr: *addr, Handler: handler}

	go func() {
		log.Printf("yasdb %s listening on %s (store=%s, durability=%s)", version, *addr, url, *durability)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	if *pprofBlockRate > 0 {
		runtime.SetBlockProfileRate(*pprofBlockRate)
	}
	if *pprofMutexFraction > 0 {
		runtime.SetMutexProfileFraction(*pprofMutexFraction)
	}

	// Metrics run on a separate server and port, not a path on the main
	// handler. This server sits on Fly's private 6PN network (see
	// fly.toml's [metrics] block), not the public-facing address, so it
	// needs its own listener. pprof rides the same private listener when
	// enabled, and is never mounted on the public -addr server.
	var metricsSrv *http.Server
	if *metricsAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			if err := srv.WriteMetrics(w); err != nil {
				http.Error(w, err.Error(), http.StatusNotImplemented)
			}
		})
		if *pprofBlockRate > 0 || *pprofMutexFraction > 0 {
			mux.HandleFunc("/debug/pprof/", pprof.Index)
			mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
			mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
			mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
			mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
			// pprof.Index serves the named profiles (goroutine, heap, block,
			// mutex, ...) via /debug/pprof/{name} on its own, but only when
			// mounted at "/debug/pprof/" on DefaultServeMux. On a custom mux,
			// each profile needs its own registration.
			for _, name := range []string{"goroutine", "heap", "threadcreate", "block", "mutex", "allocs"} {
				mux.Handle("/debug/pprof/"+name, pprof.Handler(name))
			}
			log.Printf("pprof enabled on %s/debug/pprof/ (block-rate=%d mutex-fraction=%d) — private-network-only, investigative use", *metricsAddr, *pprofBlockRate, *pprofMutexFraction)
		}
		if *adminBulkProvision {
			mux.HandleFunc("/__admin/bulk-provision", srv.HandleBulkProvision)
			log.Printf("bulk-provision enabled on %s/__admin/bulk-provision — private-network-only, load-test use", *metricsAddr)
		}
		metricsSrv = &http.Server{Addr: *metricsAddr, Handler: mux}
		go func() {
			log.Printf("yasdb metrics listening on %s (/metrics)", *metricsAddr)
			if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("metrics serve: %v", err)
			}
		}()
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(ctx)
	}
	if err := srv.Close(); err != nil {
		log.Printf("close: %v", err)
	}
}
