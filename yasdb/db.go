package yasdb

import (
	"sync"
	"time"

	"github.com/JayJamieson/yasdb/internal/ds"
)

// Config configures a DB. The zero value is valid and uses the same
// defaults ds.NewServer does.
type Config struct {
	// MaxRecordBytes caps a single append's body. 0 = 4 MiB.
	MaxRecordBytes int64

	// LongPollTimeout and SSELifetime bound the HTTP long-poll/SSE live-read
	// surface (github.com/JayJamieson/yasdb/yasdb/httpapi), not the
	// in-process LiveCursor: LiveCursor.Next blocks on its own ctx instead
	// (see Stream.Tail). They only matter if you also mount httpapi on this
	// DB. 0 = 25s / 60s.
	LongPollTimeout time.Duration
	SSELifetime     time.Duration

	// DormancyTimeout is how long an idle stream stays resident before its
	// in-memory streamer exits. 0 = 60s.
	DormancyTimeout time.Duration
	// SweepInterval is how often the TTL/expiry sweeper runs. 0 = 1s.
	SweepInterval time.Duration
	// MaxReadBytes soft-caps bytes returned per Cursor/LiveCursor chunk. 0 = 1 MiB.
	MaxReadBytes uint64

	// LiveCoalesceWindow folds a burst of commits into one leading-edge
	// debounced wake for parked live readers (long-poll/SSE/LiveCursor),
	// bounding wake rate under high-fan-out. 0 = wake on every commit.
	LiveCoalesceWindow time.Duration

	// DisableLiveCache turns off the in-memory recent-records cache that
	// lets a caught-up live reader assemble its next chunk without a store
	// scan. Correctness is identical either way.
	DisableLiveCache bool
	// LiveCacheMaxRecords and LiveCacheMaxBytes bound the per-stream cache
	// retained window (whichever binds first). 0 = defaults (512 / 256 KiB).
	LiveCacheMaxRecords int
	LiveCacheMaxBytes   int

	// Durability selects how appends are acknowledged: "" / "sync" blocks
	// each group-commit on a durable write; "notifier" writes non-durably
	// and acknowledges via a durable-seq watcher, pipelining appends. Both
	// acknowledge only once the data is durable.
	Durability           string
	NotifierPollInterval time.Duration

	// CommitterBatchMax bounds how many different streams' bursts one
	// physical commit folds together (notifier mode only). 0 = engine default.
	CommitterBatchMax int
}

func (c Config) toDS() ds.Config {
	return ds.Config{
		MaxRecordBytes:       c.MaxRecordBytes,
		LongPollTimeout:      c.LongPollTimeout,
		SSELifetime:          c.SSELifetime,
		DormancyTimeout:      c.DormancyTimeout,
		SweepInterval:        c.SweepInterval,
		MaxReadBytes:         c.MaxReadBytes,
		LiveCoalesceWindow:   c.LiveCoalesceWindow,
		DisableLiveCache:     c.DisableLiveCache,
		LiveCacheMaxRecords:  c.LiveCacheMaxRecords,
		LiveCacheMaxBytes:    c.LiveCacheMaxBytes,
		Durability:           c.Durability,
		NotifierPollInterval: c.NotifierPollInterval,
		CommitterBatchMax:    c.CommitterBatchMax,
	}
}

// DB is an open yasdb engine over a Storage backend: the registry of
// streams plus the durability/live-read machinery, same as internal/ds.
// Server. One per process/store.
type DB struct {
	srv *ds.Server

	closeOnce sync.Once
	closeErr  error
}

// Open opens a DB over store (see OpenStore for the bundled SlateDB
// backend, or implement Storage yourself). It recovers the id counter and
// resumes any interrupted deletions.
func Open(store Storage, cfg Config) (*DB, error) {
	srv, err := ds.NewServer(store, cfg.toDS())
	if err != nil {
		return nil, err
	}
	return &DB{srv: srv}, nil
}

// Close shuts down every resident stream and the underlying store. Callers
// must stop issuing calls into the DB first (an httpapi.Handler's server
// should be drained before this runs): freeing the native SlateDB store
// while a call is in flight is a use-after-free.
//
// Close is idempotent: a second and later call returns the same error the
// first one did (internal/ds.Server.Close is not itself safe to call
// twice), so a defer db.Close() alongside an earlier explicit close, or a
// graceful-shutdown path that might run twice, is safe.
func (db *DB) Close() error {
	db.closeOnce.Do(func() { db.closeErr = db.srv.Close() })
	return db.closeErr
}

// Stream returns a cheap handle for path — like bbolt's Bucket, this does
// no I/O. Every Stream method resolves state fresh on each call.
func (db *DB) Stream(path string) *Stream { return &Stream{db: db, path: path} }

// Server exposes the underlying *internal/ds.Server for other packages in
// this module (github.com/JayJamieson/yasdb/yasdb/httpapi) to build the
// optional HTTP adapter on top of the same engine. Not useful to call from
// outside this module — internal/ds is not importable there — so a normal
// embedder never touches this; pass *DB itself to httpapi.NewHandler.
func (db *DB) Server() *ds.Server { return db.srv }
