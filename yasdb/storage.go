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
