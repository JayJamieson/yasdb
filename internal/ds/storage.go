package ds

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	slatedb "slatedb.io/slatedb-go/uniffi"
)

// ResolveStore maps the store flags to a SlateDB object-store URL and the
// database path/prefix within it. SlateDB's object-store URLs must have an
// empty path (the store is a root like memory:/// or an S3 bucket). So a
// local data directory is expressed by rooting LocalFileSystem at /
// (file:///), with the absolute directory as the path prefix. An empty
// storeURL selects the local file store and creates dataDir if needed.
func ResolveStore(storeURL, dataDir, dbPath string) (url, dbPrefix string, err error) {
	if storeURL != "" {
		return storeURL, dbPath, nil
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve data dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", "", fmt.Errorf("create data dir: %w", err)
	}
	return "file:///", strings.TrimPrefix(abs, "/") + "/" + dbPath, nil
}

// slateDBLogLevels maps flag-friendly level names to slatedb.LogLevel.
var slateDBLogLevels = map[string]slatedb.LogLevel{
	"off":   slatedb.LogLevelOff,
	"error": slatedb.LogLevelError,
	"warn":  slatedb.LogLevelWarn,
	"info":  slatedb.LogLevelInfo,
	"debug": slatedb.LogLevelDebug,
	"trace": slatedb.LogLevelTrace,
}

// InitSlateDBLogging installs SlateDB's native (Rust-side) logging for the
// process, once, at the given level ("off"|"error"|"warn"|"info"|"debug"|
// "trace"). Passing a nil callback to the binding routes log records
// straight to stderr via its own tracing formatter. This is deliberately
// not bridged through a Go callback: crossing the FFI boundary per log
// line would add contention exactly where this is most useful, diagnosing
// load-under-stress stalls, where a callback bridge could itself perturb
// the thing being measured. Fly (and any other stderr-capturing
// environment) picks these up for free, alongside yasdb's own log.Print
// output.
func InitSlateDBLogging(levelName string) error {
	level, ok := slateDBLogLevels[levelName]
	if !ok {
		return fmt.Errorf("unknown slatedb log level %q (want one of off|error|warn|info|debug|trace)", levelName)
	}
	return slatedb.InitLogging(level, nil)
}

// errStoreClosed is returned by DurableSeq once the database handle is closed.
var errStoreClosed = errors.New("store closed")

// Op is a single mutation in a batch.
type Op struct {
	Key []byte
	Val []byte
	Del bool
}

// Iterator is a forward scan over a key range.
type Iterator interface {
	// Next returns the next key/value. ok is false at end of range.
	Next() (key, val []byte, ok bool, err error)
	Close()
}

// Storage is the persistence vertical. It is intentionally
// small, so a second backend stays viable. All batches are
// written atomically. A durable batch only returns once the data is
// persisted.
type Storage interface {
	// Get returns the value for key. found is false when the key is absent.
	Get(key []byte) (val []byte, found bool, err error)
	// Commit applies ops atomically. When awaitDurable is true, it blocks
	// until the write is durable in object storage.
	Commit(ops []Op, awaitDurable bool) error
	// CommitAsync applies ops atomically WITHOUT awaiting durability. It
	// returns the engine sequence number assigned to the write. The write
	// is durable once DurableSeq() reaches this value. This powers the
	// durable-seq notifier (Config.Durability = notifier).
	CommitAsync(ops []Op) (seq uint64, err error)
	// DurableSeq returns the highest engine sequence number known durable.
	DurableSeq() (uint64, error)
	// Scan returns a forward iterator over [start, end). A nil end scans
	// to the end of the keyspace.
	Scan(start, end []byte) (Iterator, error)
	Close() error
}

// slateStore is the SlateDB-backed Storage implementation.
type slateStore struct {
	db       *slatedb.Db
	store    *slatedb.ObjectStore
	recorder *slatedb.DefaultMetricsRecorder

	// sub is SlateDB v0.18's status subscription, the push source behind
	// WatchDurable. subMu guards its liveness against Close. uniffi
	// refcounts the native handle, so a WaitForChange already in flight
	// survives a Destroy, but one that STARTS after a Destroy panics with
	// "object has already been destroyed". So the property to enforce is
	// "no call may start after Destroy", which is what subClosed under the
	// write lock gives. Read-held for the length of a watch call.
	subMu     sync.RWMutex
	sub       *slatedb.DbStatusSubscription
	subClosed bool
}

// MetricsProvider is implemented by Storage backends that expose native
// engine metrics. Only slateStore does. fakeStore, used by the test suite
// by default, does not. So callers should type-assert, rather than adding
// this to the small Storage interface itself.
type MetricsProvider interface {
	MetricsSnapshot() []slatedb.Metric
}

// DurabilitySubscriber is the optional capability a Storage backend
// implements when it can block until its durable watermark advances, rather
// than being polled for it. Only slateStore does, via SlateDB v0.18's status
// subscription. pwalStore, the test fake, and any embedder-supplied backend
// are polled through DurableSeq at Config.NotifierPollInterval instead.
//
// Like MetricsProvider above, callers type-assert for this rather than
// growing the small Storage interface, which is public API that embedders
// implement themselves (yasdb/storage.go).
//
// A decorator that embeds the Storage interface does not inherit this and
// silently falls back to polling. That is correct, just slower; forward
// WatchDurable explicitly to keep the push path.
type DurabilitySubscriber interface {
	// WatchDurable blocks until the backend's durable watermark is known to
	// exceed after, then returns it. It must return promptly once stop is
	// closed, or bound itself some other way, because Server.Close joins the
	// watcher goroutine and cannot interrupt it. See watchDurable in
	// durability.go for the full contract, including why the returned
	// watermark must be an observation and never a floor the caller already
	// knew, and yasdb/storage.go for the copy an embedder can read.
	WatchDurable(after uint64, stop <-chan struct{}) (durable uint64, err error)
}

// watchTimeout bounds one wait for the durable watermark to advance, in both
// the push and the poll implementation. It is not a poll interval: the
// notifier waits only while an acknowledgement is outstanding, so an idle
// server never arms it. It is a liveness backstop, so a wedged engine
// degrades to a re-check instead of hanging.
const watchTimeout = 250 * time.Millisecond

// WatchDurable implements DurabilitySubscriber over the v0.18 subscription.
//
// stop is ignored, because WaitForChange is an uninterruptible FFI call: once
// it is entered there is nothing to select on. What bounds this instead is
// the watchTimeout the call is given, so the notifier's join waits at most
// that long. Server.Close is also ordered to call db.Shutdown() before
// destroying the subscription, which unparks a watch in flight at once
// (TestSlateStoreCloseWithWatchParked), so watchTimeout is the backstop
// rather than the usual cost.
//
// The binding reports a close as a VALUE, not an error: (status, nil) with
// status.CloseReason set. (nil, nil) is the timeout. (nil, err) is the watch
// sender being dropped. All three are folded here, so the notifier sees only
// a watermark or a closed store.
func (s *slateStore) WatchDurable(after uint64, _ <-chan struct{}) (uint64, error) {
	s.subMu.RLock()
	defer s.subMu.RUnlock()
	if s.subClosed {
		return 0, errStoreClosed
	}
	st, err := s.sub.WaitForChange(after, uint32(watchTimeout/time.Millisecond))
	if err != nil {
		return 0, errStoreClosed
	}
	if st == nil {
		// Echoing after back would be a watermark the engine never
		// reported, and the notifier carries this value forward as the
		// baseline it acknowledges against. Status() is non-blocking and
		// reads the same snapshot the subscription holds, so the timeout
		// path returns a real observation instead.
		cur := s.sub.Status()
		if cur.CloseReason != nil {
			return cur.DurableSeq, errStoreClosed
		}
		return cur.DurableSeq, nil
	}
	if st.CloseReason != nil {
		return st.DurableSeq, errStoreClosed
	}
	return st.DurableSeq, nil
}

// MetricsSnapshot returns a point-in-time snapshot of every metric SlateDB
// has registered (write/read/compaction/cache/object-store counters,
// gauges, and histograms), via the engine's built-in
// DefaultMetricsRecorder, attached in openStore. See metrics.go for how
// this gets turned into a Prometheus exposition.
func (s *slateStore) MetricsSnapshot() []slatedb.Metric {
	return s.recorder.Snapshot()
}

// StoreTuning holds the SlateDB engine knobs OpenStore exposes as flags.
// Zero values mean "use the SlateDB default" for every field.
//
// CompactorMaxConcurrentJobs and CompactorMaxSubcompactions exist because
// of a concrete, reproduced failure mode (see BENCHMARKS.md's "Compaction
// stall under load" section): a single tiered compaction merging several
// accumulated sorted runs spawns MaxSubcompactions workers that compete
// with the WAL flush for the same CPU and object-store PUT bandwidth.
// SlateDB's own backpressure and L0-stall counters do NOT fire for this,
// because it is uncontended concurrency, not a deliberate write-throttle.
// So lowering these is the available lever, trading slower catch-up
// compaction for less foreground disruption during a big merge.
type StoreTuning struct {
	// FlushInterval sets SlateDB's WAL flush cadence. An AwaitDurable
	// write blocks until the next flush, so this is the floor on
	// single-append durable latency (default 100ms in SlateDB). A
	// smaller value lowers latency at the cost of more frequent
	// object-store writes; group commit amortises it under concurrency.
	FlushInterval time.Duration
	// CompactorMaxConcurrentJobs caps how many compaction jobs run at
	// once (compactor_options.max_concurrent_compactions; SlateDB
	// default 4).
	CompactorMaxConcurrentJobs int
	// CompactorMaxSubcompactions caps how many parallel workers one
	// compaction job spawns (compactor_options.worker.max_subcompactions;
	// SlateDB default 4). This is the direct knob for the contention
	// described above.
	CompactorMaxSubcompactions int
	// L0FlushParallelism caps concurrent L0 SST flush uploads
	// (l0_flush_parallelism; SlateDB default 4).
	L0FlushParallelism int
}

// OpenStore opens (or creates) a SlateDB-backed Storage at path within the
// object store addressed by storeURL (e.g. "memory:///",
// "file:///var/lib/yasdb"), applying the given tuning (see StoreTuning).
func OpenStore(path, storeURL string, tuning StoreTuning) (Storage, error) {
	var settings *slatedb.Settings
	set := func(key, valueJSON string) error {
		if settings == nil {
			settings = slatedb.SettingsDefault()
		}
		return settings.Set(key, valueJSON)
	}
	if tuning.FlushInterval > 0 {
		if err := set("flush_interval", fmt.Sprintf(`"%dms"`, tuning.FlushInterval.Milliseconds())); err != nil {
			return nil, fmt.Errorf("set flush_interval: %w", err)
		}
	}
	if tuning.CompactorMaxConcurrentJobs > 0 {
		if err := set("compactor_options.max_concurrent_compactions", fmt.Sprintf("%d", tuning.CompactorMaxConcurrentJobs)); err != nil {
			return nil, fmt.Errorf("set compactor_options.max_concurrent_compactions: %w", err)
		}
	}
	if tuning.CompactorMaxSubcompactions > 0 {
		if err := set("compactor_options.worker.max_subcompactions", fmt.Sprintf("%d", tuning.CompactorMaxSubcompactions)); err != nil {
			return nil, fmt.Errorf("set compactor_options.worker.max_subcompactions: %w", err)
		}
	}
	if tuning.L0FlushParallelism > 0 {
		if err := set("l0_flush_parallelism", fmt.Sprintf("%d", tuning.L0FlushParallelism)); err != nil {
			return nil, fmt.Errorf("set l0_flush_parallelism: %w", err)
		}
	}
	if strings.HasPrefix(storeURL, "file://") {
		// LocalFileSystem never implements the conditional overwrite
		// (PutMode::Update) that SlateDB's garbage collector normally uses
		// to safely advance manifest/compaction boundary files. Without
		// this setting, every GC sweep against a file:// store errors with
		// NotImplemented on every run (visible as repeated "error
		// collecting garbage" log lines and a nonzero
		// object_store_error_count{op="put"}), so GC never actually
		// reclaims anything. boundary_files_enabled=false is SlateDB's
		// documented mechanism for object stores without conditional
		// overwrites: GC still runs and reclaims space, just without the
		// extra CAS-based guard against a stale or paused process resuming
		// mid-update. SlateDB's own min_age default (300s) is the fallback
		// protection its docs recommend instead, so it is left untouched
		// here.
		if err := set("garbage_collector_options.boundary_files_enabled", "false"); err != nil {
			return nil, fmt.Errorf("set garbage_collector_options.boundary_files_enabled: %w", err)
		}
	}
	return openStore(path, storeURL, settings)
}

// openStore opens (or creates) a SlateDB database at path within the object
// store addressed by storeURL (e.g. "memory:///", "file:///var/lib/yasdb").
func openStore(path, storeURL string, settings *slatedb.Settings) (*slateStore, error) {
	store, err := slatedb.ObjectStoreResolve(storeURL)
	if err != nil {
		return nil, err
	}
	builder := slatedb.NewDbBuilder(path, store)
	defer builder.Destroy()
	if settings != nil {
		if err := builder.WithSettings(settings); err != nil {
			store.Destroy()
			return nil, err
		}
	}
	recorder := slatedb.NewDefaultMetricsRecorder()
	if err := builder.WithMetricsRecorder(recorder); err != nil {
		recorder.Destroy()
		store.Destroy()
		return nil, err
	}
	db, err := builder.Build()
	if err != nil {
		recorder.Destroy()
		store.Destroy()
		return nil, err
	}
	return &slateStore{db: db, store: store, recorder: recorder, sub: db.Subscribe()}, nil
}

var readOpts = slatedb.ReadOptions{
	DurabilityFilter: slatedb.DurabilityLevelMemory, // include not-yet-remote writes
	Dirty:            false,
	CacheBlocks:      true,
}

var scanOpts = slatedb.ScanOptions{
	DurabilityFilter: slatedb.DurabilityLevelMemory,
	Dirty:            false,
	ReadAheadBytes:   4 * 1024,
	CacheBlocks:      true,
	MaxFetchTasks:    1,
}

func (s *slateStore) Get(key []byte) ([]byte, bool, error) {
	v, err := s.db.GetWithOptions(key, readOpts)
	if err != nil {
		return nil, false, err
	}
	if v == nil {
		return nil, false, nil
	}
	return *v, true, nil
}

func (s *slateStore) Commit(ops []Op, awaitDurable bool) error {
	batch := slatedb.NewWriteBatch()
	defer batch.Destroy()
	for _, op := range ops {
		if op.Del {
			if err := batch.Delete(op.Key); err != nil {
				return err
			}
			continue
		}
		if err := batch.Put(op.Key, op.Val); err != nil {
			return err
		}
	}
	h, err := s.db.WriteWithOptions(batch, slatedb.WriteOptions{})
	if h != nil {
		defer h.Destroy()
	}
	if err != nil {
		return err
	}
	if awaitDurable {
		return h.AwaitDurable()
	}
	return nil
}

func (s *slateStore) CommitAsync(ops []Op) (uint64, error) {
	batch := slatedb.NewWriteBatch()
	defer batch.Destroy()
	for _, op := range ops {
		if op.Del {
			if err := batch.Delete(op.Key); err != nil {
				return 0, err
			}
			continue
		}
		if err := batch.Put(op.Key, op.Val); err != nil {
			return 0, err
		}
	}
	h, err := s.db.WriteWithOptions(batch, slatedb.WriteOptions{})
	if h != nil {
		defer h.Destroy()
	}
	if err != nil {
		return 0, err
	}
	return h.Seqnum(), nil
}

func (s *slateStore) DurableSeq() (uint64, error) {
	st := s.db.Status()
	if st.CloseReason != nil {
		return st.DurableSeq, errStoreClosed
	}
	return st.DurableSeq, nil
}

func (s *slateStore) Scan(start, end []byte) (Iterator, error) {
	kr := slatedb.KeyRange{StartInclusive: true, EndInclusive: false}
	if start != nil {
		st := start
		kr.Start = &st
	}
	if end != nil {
		en := end
		kr.End = &en
	}
	it, err := s.db.ScanWithOptions(kr, scanOpts)
	if err != nil {
		return nil, err
	}
	return &slateIter{it: it}, nil
}

// Close shuts the engine down before destroying anything. Shutdown is the
// only call that unparks an in-flight WaitForChange: db.Destroy() leaves a
// parked waiter parked indefinitely. Destroying the subscription after that
// is therefore provably not racing a live call, and subClosed stops a later
// one from starting on a freed handle.
func (s *slateStore) Close() error {
	err := s.db.Shutdown()
	s.subMu.Lock()
	s.subClosed = true
	s.sub.Destroy()
	s.subMu.Unlock()
	s.db.Destroy()
	s.store.Destroy()
	s.recorder.Destroy()
	return err
}

// scanBatchSize is how many rows slateIter pulls per NextBatch call. Every
// real I/O method on the SlateDB binding, including the old per-record
// Next(), crosses the FFI boundary as an async Rust future. This is
// expensive per call, independent of how much data comes back (see
// BENCHMARKS.md's read allocation profile: 87% of a 100-record catch-up
// read's allocations were this crossing, once per record). NextBatch
// (added in slatedb-go v0.15.0) amortizes that cost over up to this many
// rows per crossing, instead of one. 64 matches maxBurst's existing
// write-side batching convention. It is not load-bearing beyond "small
// enough to bound one batch's memory, large enough that most catch-up
// reads finish in a single NextBatch call."
const scanBatchSize = 64

// slateIter buffers NextBatch results, so Iterator.Next(), used unmodified
// by every caller (read.go, deleter.go, expiry.go), gets the batched-FFI
// win transparently, with no interface or caller changes.
type slateIter struct {
	it        *slatedb.DbIterator
	buf       []slatedb.KeyValue
	idx       int
	exhausted bool
}

func (si *slateIter) Next() (key, val []byte, ok bool, err error) {
	if si.idx >= len(si.buf) {
		if si.exhausted {
			return nil, nil, false, nil
		}
		si.buf, err = si.it.NextBatch(scanBatchSize)
		if err != nil {
			return nil, nil, false, err
		}
		si.idx = 0
		// Per NextBatch's contract, a vector shorter than the requested max
		// (including empty) means the iterator is exhausted. This skips
		// the otherwise-wasted trailing call that would just re-confirm
		// that.
		if len(si.buf) < scanBatchSize {
			si.exhausted = true
		}
		if len(si.buf) == 0 {
			return nil, nil, false, nil
		}
	}
	kv := si.buf[si.idx]
	si.idx++
	return kv.Key, kv.Value, true, nil
}

func (si *slateIter) Close() {
	if si.it != nil {
		si.it.Destroy()
		si.it = nil
	}
}
