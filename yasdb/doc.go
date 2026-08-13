// Package yasdb is yasdb's embeddable Go API: an in-process Durable
// Streams engine with no net/http in its core path. See
// docs/rfcs/0002-embeddable-library-api.md for the design behind this
// package's shape.
//
// Open a DB over a Storage (SlateDB via OpenStore, or a caller's own
// Storage implementation), get a cheap Stream handle by path, and
// Create/Append/Read/Tail it directly:
//
//	store, _ := yasdb.OpenStore("mydb", "memory:///", yasdb.StoreTuning{})
//	db, _ := yasdb.Open(store, yasdb.Config{})
//	defer db.Close()
//
//	s := db.Stream("/orders")
//	_ = s.Create(ctx, "application/json")
//	_, _ = s.AppendJSON(ctx, map[string]int{"id": 1})
//
//	cur, _ := s.Read(ctx, yasdb.Offset{})
//	body, _, _, _ := cur.Next(ctx)
//
// A Go server that also wants the wire protocol (for non-Go clients, or a
// browser via SSE/long-poll) layers github.com/JayJamieson/yasdb/yasdb/httpapi
// on top of the same *DB — that package's Handler mounts on any existing
// Go muxer.
package yasdb
