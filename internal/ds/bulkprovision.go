package ds

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// defaultBulkProvisionBatch caps how many streams one Storage.Commit call
// creates at once. This is the whole point of this endpoint. createStream
// holds metaMu across its entire synchronous Commit(ops, true). So N
// individual PUTs, even issued concurrently, serialize on that mutex, and
// each pays a full flush-interval wait (see BENCHMARKS.md's flush-cadence
// sweep, where sequential provisioning wall-clock scaled linearly with
// -flush). Batching many creates into one Commit pays that wait once per
// batch, instead of once per stream.
const defaultBulkProvisionBatch = 2000

type bulkProvisionRequest struct {
	PathPrefix  string `json:"pathPrefix"`
	Count       int    `json:"count"`
	ContentType string `json:"contentType"`
	BatchSize   int    `json:"batchSize"`
}

type bulkProvisionResponse struct {
	Created int `json:"created"`
	Batches int `json:"batches"`
}

// HandleBulkProvision creates Count empty streams named "<PathPrefix><i>"
// for i in [0, Count), directly against storage instead of through Count
// individual HTTP requests. This is meant for load-test setup, not general
// protocol use. Unlike PUT-created streams, it skips the
// tombstone/idempotency checks createStream does per path, on the
// assumption paths are fresh. Callers are expected to ensure that
// themselves (e.g. benchmark/loadshape prefixes every run with a unique
// run id — see ../../benchmark/README.md).
//
// This is mounted only on -metrics-addr (private-network-only), behind
// -admin-bulk-provision, matching the pprof endpoints' opt-in/private
// precedent in main.go: it is a mutating, storage-filling operation with no
// place on the public address.
func (s *Server) HandleBulkProvision(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req bulkProvisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.PathPrefix == "" || req.Count <= 0 {
		http.Error(w, "pathPrefix and a positive count are required", http.StatusBadRequest)
		return
	}
	if req.ContentType == "" {
		req.ContentType = defaultContentType
	}
	batch := req.BatchSize
	if batch <= 0 {
		batch = defaultBulkProvisionBatch
	}

	batches := 0
	for start := 0; start < req.Count; start += batch {
		n := batch
		if start+n > req.Count {
			n = req.Count - start
		}
		if err := s.bulkProvisionBatch(req.PathPrefix, start, n, req.ContentType); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		batches++
	}

	w.Header().Set(hContentType, "application/json")
	w.Header().Set(hCacheControl, "no-store")
	json.NewEncoder(w).Encode(bulkProvisionResponse{Created: req.Count, Batches: batches})
}

// bulkProvisionBatch creates n empty streams "<prefix><start>" ..
// "<prefix><start+n-1>" in a single Commit, under one metaMu critical
// section. This is the same lock discipline createStream uses for one
// stream, amortized here over many.
func (s *Server) bulkProvisionBatch(prefix string, start, n int, contentType string) error {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()

	now := time.Now()
	id := s.nextID
	ops := make([]Op, 0, n*3+1)
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("%s%d", prefix, start+i)
		sid := id + uint64(i)
		meta := &StreamMeta{
			StreamID:    sid,
			ContentType: contentType,
			CreatedAtMs: now.UnixMilli(),
		}
		ops = append(ops,
			Op{Key: metaKey(path), Val: meta.Marshal()},
			Op{Key: idMappingKey(sid), Val: []byte(path)},
			Op{Key: tailKey(sid), Val: marshalTail(0, now.Unix())},
		)
	}
	ops = append(ops, Op{Key: idCounterKey(), Val: u64Value(id + uint64(n))})

	if err := s.store.Commit(ops, true); err != nil {
		return err
	}
	s.nextID = id + uint64(n)
	return nil
}
