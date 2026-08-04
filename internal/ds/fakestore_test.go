package ds

import (
	"os"
	"sort"
	"sync"
	"time"
)

// fakeStore is an in-memory Storage used by the functional tests: a second
// implementation of the persistence seam that needs no cgo/SlateDB and
// behaves deterministically. Keys sort byte-wise, matching SlateDB, which
// the big-endian key encoding relies on. Commit is atomic, and writes are
// durable the instant they land. So CommitAsync assigns a monotonic seq
// that DurableSeq immediately reflects, exercising the notifier durability
// path without real flush latency.
//
// Set YASDB_TEST_BACKEND=slatedb to run the same suite against the real store.
type fakeStore struct {
	mu     sync.Mutex
	data   map[string][]byte
	seq    uint64
	closed bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{data: make(map[string][]byte)}
}

// openTestStore returns the store the functional tests run against: the in-memory
// fake by default, or the real SlateDB store when YASDB_TEST_BACKEND=slatedb.
func openTestStore(tb interface {
	Helper()
	Fatalf(string, ...any)
}, flush time.Duration) Storage {
	tb.Helper()
	switch os.Getenv("YASDB_TEST_BACKEND") {
	case "slatedb":
		path := uniqueDBPath("test")
		store, err := OpenStore(path, "memory:///", StoreTuning{FlushInterval: flush})
		if err != nil {
			tb.Fatalf("open store: %v", err)
		}
		return store
	case "pwal":
		dir, err := os.MkdirTemp("", "yasdb-pwal-test-*")
		if err != nil {
			tb.Fatalf("mkdir temp: %v", err)
		}
		store, err := OpenPWALStore(dir, 4)
		if err != nil {
			tb.Fatalf("open pwal store: %v", err)
		}
		return store
	}
	return newFakeStore()
}

func (f *fakeStore) apply(ops []Op) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, op := range ops {
		k := string(op.Key)
		if op.Del {
			delete(f.data, k)
			continue
		}
		f.data[k] = append([]byte(nil), op.Val...)
	}
	f.seq++
	return f.seq
}

func (f *fakeStore) Get(key []byte) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.data[string(key)]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), v...), true, nil
}

func (f *fakeStore) Commit(ops []Op, _ bool) error {
	f.apply(ops)
	return nil
}

func (f *fakeStore) CommitAsync(ops []Op) (uint64, error) {
	return f.apply(ops), nil
}

func (f *fakeStore) DurableSeq() (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return f.seq, errStoreClosed
	}
	return f.seq, nil
}

// Scan snapshots the matching keys under lock so an in-flight iteration is a
// consistent view, matching the engine's snapshot-read semantics.
func (f *fakeStore) Scan(start, end []byte) (Iterator, error) {
	lo, hi := string(start), string(end)
	f.mu.Lock()
	keys := make([]string, 0, len(f.data))
	for k := range f.data {
		if start != nil && k < lo {
			continue
		}
		if end != nil && k >= hi {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	kvs := make([]kv, len(keys))
	for i, k := range keys {
		kvs[i] = kv{key: []byte(k), val: append([]byte(nil), f.data[k]...)}
	}
	f.mu.Unlock()
	return &fakeIter{kvs: kvs}, nil
}

func (f *fakeStore) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

type kv struct{ key, val []byte }

type fakeIter struct {
	kvs []kv
	i   int
}

func (it *fakeIter) Next() (key, val []byte, ok bool, err error) {
	if it.i >= len(it.kvs) {
		return nil, nil, false, nil
	}
	e := it.kvs[it.i]
	it.i++
	return e.key, e.val, true, nil
}

func (it *fakeIter) Close() { it.kvs = nil }
