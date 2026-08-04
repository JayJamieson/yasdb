package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/JayJamieson/yasdb/internal/state"
)

// stateSnapshotPrefix is the reserved path that serves the materialized-state
// convenience endpoint, mirroring /__health.
const stateSnapshotPrefix = "/__state"

// recorder is a minimal http.ResponseWriter. It captures a response
// in-process, for the self-request serveStateSnapshot issues against the
// running Server. It is hand-rolled on purpose, instead of importing
// net/http/httptest, which is a test-only convention in this repo.
type recorder struct {
	statusCode int
	header     http.Header
	body       []byte
}

func (r *recorder) Header() http.Header {
	if r.header == nil {
		r.header = make(http.Header)
	}
	return r.header
}

func (r *recorder) Write(b []byte) (int, error) {
	r.body = append(r.body, b...)
	return len(b), nil
}

func (r *recorder) WriteHeader(code int) {
	r.statusCode = code
}

// entity is one materialized entity in a snapshot response.
type entity struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// snapshotResponse is the body of a GET /__state/<path> response: the current
// materialized state of the target stream, grouped by entity type.
type snapshotResponse struct {
	GeneratedAt      string              `json:"generatedAt"`
	StreamNextOffset string              `json:"streamNextOffset"`
	Collections      map[string][]entity `json:"collections"`
}

// serveStateSnapshot implements a State Protocol materialized-snapshot
// endpoint at /__state/<stream-path>. It replays a stream's full history
// through a state.Materializer and returns the current entity state grouped
// by type, plus the Stream-Next-Offset to resume live-tailing from. This
// saves a client from replaying arbitrarily long raw history itself, just to
// get an initial view. DESIGN.md explains why this lives here and not in
// internal/ds: the State Protocol is a message-format convention, and the
// base stream engine does not need to know about it.
func serveStateSnapshot(inner http.Handler, w http.ResponseWriter, r *http.Request) {
	streamPath := strings.TrimPrefix(r.URL.Path, stateSnapshotPrefix)
	if streamPath == "" || streamPath == "/" {
		http.Error(w, "missing stream path: use /__state/<stream-path>", http.StatusBadRequest)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, streamPath+"?offset=-1", nil)
	if err != nil {
		http.Error(w, "build internal request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Accept", "application/json")
	rec := &recorder{}
	inner.ServeHTTP(rec, req)

	if rec.statusCode == http.StatusNotFound {
		http.Error(w, "stream not found", http.StatusNotFound)
		return
	}
	if rec.statusCode != 0 && rec.statusCode != http.StatusOK {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(rec.statusCode)
		w.Write(rec.body)
		return
	}

	var msgs []state.Message
	if len(strings.TrimSpace(string(rec.body))) > 0 {
		if err := json.Unmarshal(rec.body, &msgs); err != nil {
			http.Error(w, "stream is not a State Protocol JSON message stream: "+err.Error(), http.StatusUnprocessableEntity)
			return
		}
	}

	mat := state.NewMaterializer()
	mat.ApplyAll(msgs) // malformed individual messages are skipped, not fatal

	snap := mat.Snapshot()
	collections := make(map[string][]entity, len(snap))
	for typ, coll := range snap {
		list := make([]entity, 0, len(coll))
		for key, value := range coll {
			list = append(list, entity{Key: key, Value: value})
		}
		collections[typ] = list
	}

	resp := snapshotResponse{
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		StreamNextOffset: rec.header.Get("Stream-Next-Offset"),
		Collections:      collections,
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(resp)
}
