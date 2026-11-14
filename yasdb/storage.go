package yasdb

import "github.com/JayJamieson/yasdb/internal/ds"

// Storage is the persistence vertical a DB is opened over. It is
// intentionally small enough for a caller to implement their own backend;
// OpenStore returns the bundled SlateDB-backed implementation. This is a
// type alias for internal/ds.Storage (identical type, just spellable
// outside this module), so a type satisfying Storage also satisfies
// ds.Storage without any adapter.
type Storage = ds.Storage

// Op is a single mutation in a Storage.Commit batch.
type Op = ds.Op

// Iterator is a forward scan over a Storage key range, from Storage.Scan.
type Iterator = ds.Iterator

// MetricsProvider is implemented by Storage backends that expose native
// engine metrics (OpenStore's SlateDB backend does). Type-assert a Storage
// value against this rather than adding it to Storage itself, since not
// every backend has metrics to report.
type MetricsProvider = ds.MetricsProvider

// DurabilitySubscriber is implemented by Storage backends that can block
// until their durable watermark advances (OpenStore's SlateDB backend does,
// via its status subscription). In notifier durability mode a Storage that
// implements this is pushed to rather than polled every
// Config.NotifierPollInterval. Implementing it is optional: a backend
// without it keeps the poll.
//
// The interface has one method:
//
//	WatchDurable(after uint64, stop <-chan struct{}) (durable uint64, err error)
//
// An implementation must hold all of the following.
//
//   - It blocks until the backend's durable watermark exceeds after, then
//     returns that watermark.
//   - It may instead return early with a watermark at or below after,
//     meaning "nothing new yet". yasdb loops.
//   - The watermark it returns is always one the backend observed as
//     durable, never after itself and never a floor the caller already knew.
//     yasdb carries the value forward as the baseline it acknowledges
//     against, so a watermark the backend never reached would acknowledge a
//     write that is not durable.
//   - It returns promptly once stop is closed. DB.Close joins the watcher
//     goroutine before it calls Storage.Close, and it has no other way to
//     interrupt a call already in flight, so a WatchDurable that waits on a
//     condition and ignores stop hangs DB.Close for as long as that
//     condition takes. Returning the current watermark with a nil error is
//     enough. A call that cannot watch stop, such as one parked in a C
//     library, must bound itself with its own timeout instead.
//   - A non-nil error means the backend is closed or has failed. yasdb then
//     fails every outstanding acknowledgement and stops watching, and the
//     returned watermark is ignored.
//
// yasdb calls WatchDurable from one goroutine only, and that is the same
// goroutine it runs durability callbacks on, so an implementation need not be
// safe for concurrent use.
//
// A decorator that embeds the Storage interface does not inherit this and
// falls back to the poll. That is correct, just slower. Forward WatchDurable
// explicitly to keep the push path.
type DurabilitySubscriber = ds.DurabilitySubscriber

// StoreTuning holds the SlateDB engine knobs OpenStore exposes. Zero
// values mean "use the SlateDB default" for every field.
type StoreTuning = ds.StoreTuning

// OpenStore opens (or creates) the bundled SlateDB-backed Storage at path
// within the object store addressed by storeURL (e.g. "memory:///",
// "file:///var/lib/yasdb", "s3://bucket"), applying tuning.
func OpenStore(path, storeURL string, tuning StoreTuning) (Storage, error) {
	return ds.OpenStore(path, storeURL, tuning)
}

// ResolveStore maps a local-directory-style configuration (an empty
// storeURL, a local dataDir, and a db path/prefix) to the SlateDB
// object-store URL and path prefix OpenStore expects. SlateDB object-store
// URLs must have no path component, so a local directory needs this
// translation; a real object-store URL (memory:///, s3://bucket) passes
// through storeURL unchanged and needs no call to this.
func ResolveStore(storeURL, dataDir, dbPath string) (url, dbPrefix string, err error) {
	return ds.ResolveStore(storeURL, dataDir, dbPath)
}
