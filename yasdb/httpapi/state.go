package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/JayJamieson/yasdb/yasdb"
)

// entity is one materialized entity in a snapshot response.
type entity struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// snapshotResponse is the body of a GET /__state/<path> response: the
// current materialized state of the target stream, grouped by entity type.
type snapshotResponse struct {
	GeneratedAt      string              `json:"generatedAt"`
	StreamNextOffset string              `json:"streamNextOffset"`
	Collections      map[string][]entity `json:"collections"`
}

// serveStateSnapshot implements the State Protocol materialized-snapshot
// endpoint at /__state/<stream-path>. It replays a stream's full history
// through a state.Materializer (via yasdb.Stream.Materialize, an
// in-process call now) and returns the current entity state grouped by
// type, plus the offset to resume live-tailing from via the wire
// protocol's ?live=sse endpoint. This saves a client from replaying
// arbitrarily long raw history itself just to get an initial view.
//
// This used to be implemented as a self-request: a real GET ?offset=-1
// built against Server.ServeHTTP through a hand-rolled http.ResponseWriter
// recorder, done that way because there was nothing else in the module to
// depend on for "replay a stream and materialize it" outside of the HTTP
// surface itself. Stream.Materialize (RFC 0002) is that direct call.
func serveStateSnapshot(db *yasdb.DB, w http.ResponseWriter, r *http.Request) {
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

	mat, next, err := db.Stream(streamPath).Materialize(r.Context())
	if err != nil {
		writeMaterializeError(w, err)
		return
	}

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
		StreamNextOffset: next.String(),
		Collections:      collections,
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(resp)
}

func writeMaterializeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, yasdb.ErrNotFound):
		http.Error(w, "stream not found", http.StatusNotFound)
	case errors.Is(err, yasdb.ErrGone):
		http.Error(w, "stream gone", http.StatusGone)
	case errors.Is(err, yasdb.ErrNotStateProtocolStream):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
