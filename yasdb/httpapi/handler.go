// Package httpapi is the optional HTTP adapter for yasdb's embeddable
// core (github.com/JayJamieson/yasdb/yasdb): it exposes a *yasdb.DB as the
// full Durable Streams wire protocol. Nothing in the core imports
// net/http; an embedder who wants zero HTTP never imports this package.
package httpapi

import (
	"net/http"
	"strings"

	"github.com/JayJamieson/yasdb/yasdb"
)

// stateSnapshotPrefix is the reserved path that serves the materialized-
// state convenience endpoint (state.go), kept out of the stream keyspace
// the same way /__health is.
const stateSnapshotPrefix = "/__state"

// NewHandler adapts db to the full Durable Streams HTTP surface: the wire
// protocol (create/append/read/tail/fork/delete, ds.Server.ServeHTTP)
// plus two convenience endpoints outside the stream keyspace —
// GET /__health (liveness/readiness) and GET /__state/<path> (the State
// Protocol materialized-snapshot endpoint, backed directly by
// yasdb.Stream.Materialize now, not a self-request).
//
// The result is a plain http.Handler, so it mounts on any Go muxer the
// normal way: mux.Handle("/", h) on a stdlib http.ServeMux, or under a
// prefix with http.StripPrefix. It expects to own every path under
// wherever it's mounted — stream paths are arbitrary strings, so anything
// that isn't /__health or /__state/... is routed as a stream path, not
// matched against a fixed route table.
func NewHandler(db *yasdb.DB) http.Handler {
	srv := db.Server()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/__health":
			serveHealth(w, r)
		case r.URL.Path == stateSnapshotPrefix || strings.HasPrefix(r.URL.Path, stateSnapshotPrefix+"/"):
			serveStateSnapshot(db, w, r)
		default:
			srv.ServeHTTP(w, r)
		}
	})
}

func serveHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write([]byte("ok\n"))
	}
}
