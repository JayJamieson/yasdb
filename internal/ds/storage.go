package ds

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	slatedb "slatedb.io/slatedb-go/uniffi"
)

// ResolveStore maps the store flags to a SlateDB object-store URL and the
// database path/prefix within it. SlateDB's object-store URLs must have an empty
// path (the store is a root like memory:/// or an S3 bucket), so a local data
// directory is expressed by rooting LocalFileSystem at / (file:///) with the
// absolute directory as the path prefix. An empty storeURL selects the local
// file store and creates dataDir if needed.
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

// Storage is the persistence vertical (SPEC.md §1). It is intentionally small so a
// second backend (e.g. objwal) stays viable. All batches are written
// atomically; a durable batch only returns once the data is persisted.
type Storage interface {
	// Get returns the value for key. found is false when the key is absent.
	Get(key []byte) (val []byte, found bool, err error)
	// Commit applies ops atomically. When awaitDurable is true it blocks until
	// the write is durable in object storage.
	Commit(ops []Op, awaitDurable bool) error
	// CommitAsync applies ops atomically WITHOUT awaiting durability, returning
	// the engine sequence number assigned to the write. The write is durable
	// once DurableSeq() reaches this value. Powers the durable-seq notifier
	// (Config.Durability = notifier).
	CommitAsync(ops []Op) (seq uint64, err error)
	// DurableSeq returns the highest engine sequence number known durable.
	DurableSeq() (uint64, error)
	// Scan returns a forward iterator over [start, end). A nil end scans to the
	// end of the keyspace.
	Scan(start, end []byte) (Iterator, error)
	Close() error
}

// slateStore is the SlateDB-backed Storage implementation.
type slateStore struct {
	db    *slatedb.Db
	store *slatedb.ObjectStore
}

// OpenStore opens (or creates) a SlateDB-backed Storage at path within the
// object store addressed by storeURL (e.g. "memory:///", "file:///var/lib/yasdb").
//
// flushInterval sets SlateDB's WAL flush cadence: an AwaitDurable write blocks
// until the next flush, so this is the floor on single-append durable latency
// (default 100ms in SlateDB). A smaller value lowers latency at the cost of more
// frequent object-store writes; group commit amortises it under concurrency.
// Zero uses the SlateDB default.
func OpenStore(path, storeURL string, flushInterval time.Duration) (Storage, error) {
	var settings *slatedb.Settings
	if flushInterval > 0 {
		settings = slatedb.SettingsDefault()
		if err := settings.Set("flush_interval", fmt.Sprintf(`"%dms"`, flushInterval.Milliseconds())); err != nil {
			return nil, fmt.Errorf("set flush_interval: %w", err)
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
	db, err := builder.Build()
	if err != nil {
		store.Destroy()
		return nil, err
	}
	return &slateStore{db: db, store: store}, nil
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
	_, err := s.db.WriteWithOptions(batch, slatedb.WriteOptions{AwaitDurable: awaitDurable})
	return err
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
	h, err := s.db.WriteWithOptions(batch, slatedb.WriteOptions{AwaitDurable: false})
	if err != nil {
		return 0, err
	}
	return h.Seqnum, nil
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

func (s *slateStore) Close() error {
	err := s.db.Shutdown()
	s.db.Destroy()
	s.store.Destroy()
	return err
}

type slateIter struct {
	it *slatedb.DbIterator
}

func (si *slateIter) Next() (key, val []byte, ok bool, err error) {
	kv, err := si.it.Next()
	if err != nil {
		return nil, nil, false, err
	}
	if kv == nil {
		return nil, nil, false, nil
	}
	return kv.Key, kv.Value, true, nil
}

func (si *slateIter) Close() {
	if si.it != nil {
		si.it.Destroy()
		si.it = nil
	}
}
