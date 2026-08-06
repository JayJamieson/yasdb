# RFC 0002: An embeddable Go library API

Design only — no code moves as part of this RFC. It exists so the shape
can be reviewed before anything gets built.

## Problem

`internal/ds.Server`'s only real entry point is `ServeHTTP(w, r)` — an
`http.Handler`. Every capability (append, read, tail, close, fork, delete,
state materialization) is reachable only by constructing an `http.Request`
and parsing status codes/headers back out of an `http.ResponseWriter`.
This project's own code pays that tax internally:

- `bench_test.go`'s `mustCreate`/`appendOnce` build raw `httptest.NewRequest`s
  and check `rr.Code`, because there is no other way to call in.
- `state.go`'s `GET /__state/<path>` handler — the feature this RFC is
  partly motivated by — reads a stream's own history by constructing a
  **fake `http.ResponseWriter`** and calling `Server.ServeHTTP` on itself
  (a real `GET ?offset=-1` against its own handler), specifically so it
  can stay "outside `internal/ds`" and only depend on the HTTP surface.
  That workaround exists *because there was nothing else to depend on*.

Two separate problems, one fix:

1. **Embedding is HTTP-shaped even for in-process callers.** A Go server
   that wants yasdb's engine in-process (no `net/http` involved at all)
   currently cannot do that.
2. **The State Protocol's snapshot mechanism (`ControlSnapshotStart`/
   `ControlSnapshotEnd`, `internal/state`) is only reachable through the
   self-request hack above**, not a direct call.

## What's actually needed: less than it looks like

Checking what `internal/ds` already exposes on `*Server` (unexported, but
real Go methods, not HTTP-only code paths) turned up almost everything
this needs already exists, just under internal names and internal request/
response structs:

| Capability | Already exists as | Called today by |
| --- | --- | --- |
| Append | `Server.submitAppend(path string, req appendReq, hint *streamer) (appendResp, bool)` | `handlers.go`'s `handlePost`, **and already directly by `bench_test.go`** |
| Create | `Server.createStream(p createParams) createResult` | `handlers.go`'s `handlePut` |
| Catch-up read | `Server.readRange(streamID, forkedFrom, forkOffset uint64, isJSON bool, start Offset, tailSeq, limit uint64) (body []byte, next Offset, upToDate bool, err error)` | `read.go`'s `readCatchup` |
| Live tail wakeup | `streamer.waiterChan() <-chan struct{}` (closes on commit) | `read.go`'s `readLongPoll`/`readSSE` |
| Current tail/closed | `Server.liveState(path string) (tail uint64, closed bool, st *streamer, found bool, err error)` | `read.go`, `admin.go` |
| Fork | `Server.createFork(p forkParams) createResult` | `handlers.go` |
| Delete | `Server.deleteStreamLocked`/`softDeleteLocked` (`expiry.go`) | `handlers.go`'s `handleDelete` |

So this is **overwhelmingly a wrapping exercise, not new engine work**:
translate already-tested internal methods and their internal
request/response structs into a small, typed public surface. The risk
profile is low precisely because the engine logic underneath doesn't
change.

## Design goals

1. **Small.** bbolt — a widely embedded Go store — has about 9 exported
   types. This should be in that neighborhood, not `internal/ds`'s current
   ~55 exported-from-internal symbols (most of which are incidental:
   `KeyType`, `ResolveStore`, `StoreTuning`, `MetricsProvider`, etc. — not
   things a day-one embedder needs).
2. **In-process by default.** No `net/http` in the core path. Errors are
   typed Go values, not status codes to decode.
3. **HTTP becomes optional, layered on top**, not the only door in.
4. **State Protocol is a first-class call**, not a self-request.
5. **No rewrite.** `internal/ds`/`internal/state` keep their current
   logic (streamer, committer, registry, materializer) untouched. A new
   *public* package in this module can import `internal/ds` freely — Go's
   `internal/` visibility rule only restricts other *modules* from
   importing it directly, not other packages inside this same module — so
   nothing needs to move out of `internal/` for this to work.

## Proposed package layout

```
github.com/JayJamieson/yasdb/yasdb        (package yasdb)   — the core, this RFC
github.com/JayJamieson/yasdb/yasdb/httpapi (package httpapi) — optional HTTP adapter
github.com/JayJamieson/yasdb/internal/ds                    — unchanged
github.com/JayJamieson/yasdb/internal/state                 — unchanged
```

(`yasdb/yasdb` reads oddly as a path, matching bbolt's own
`go.etcd.io/bbolt` → `bolt.Open(...)` convention gives the nicest call
site: `yasdb.Open(...)`. Alternatives — `streams`, `embed` — are a small
naming choice, not an architectural one; flagged as an open question
below rather than decided here.)

## Proposed API

```go
package yasdb

// DB wraps a Storage (internal/ds.Storage — already backend-agnostic:
// SlateDB, the file-backed pwal experiment, or a caller's own
// implementation) and the stream registry. One per process/store, same
// as internal/ds.Server today.
type DB struct{ /* wraps *ds.Server */ }

func Open(store ds.Storage, cfg Config) (*DB, error)
func (db *DB) Close() error

// Stream is a cheap handle — like bbolt's Bucket, opening it does no I/O.
func (db *DB) Stream(path string) *Stream

type Stream struct{ /* db + path */ }

func (s *Stream) Create(ctx context.Context, contentType string) error
func (s *Stream) Append(ctx context.Context, records ...[]byte) (Offset, error)
func (s *Stream) AppendJSON(ctx context.Context, v ...any) (Offset, error) // marshals + appends
func (s *Stream) Close(ctx context.Context) error   // EOF the stream (protocol Stream-Closed)
func (s *Stream) Delete(ctx context.Context) error
func (s *Stream) Fork(ctx context.Context, at Offset) (*Stream, error)

// Read is a bounded catch-up scan from an offset — wraps readRange.
func (s *Stream) Read(ctx context.Context, from Offset) (*Cursor, error)
type Cursor struct{ /* ... */ }
func (c *Cursor) Next(ctx context.Context) (record []byte, next Offset, ok bool, err error)

// Tail is a live read — wraps liveState + streamer.waiterChan, no SSE/
// long-poll wire format needed in-process.
func (s *Stream) Tail(ctx context.Context, from Offset) (*LiveCursor, error)
type LiveCursor struct{ /* ... */ }
func (c *LiveCursor) Next(ctx context.Context) (record []byte, next Offset, err error) // blocks until data or ctx.Done

// State Protocol — the actual motivation for this RFC. Replaces state.go's
// self-HTTP-request with a direct loop over Read + state.Materializer.
func (s *Stream) Materialize(ctx context.Context) (*state.Materializer, Offset, error)
func (s *Stream) TailState(ctx context.Context, from Offset) (*StateCursor, error)
type StateCursor struct{ /* wraps LiveCursor + json.Unmarshal into state.Message */ }
func (c *StateCursor) Next(ctx context.Context) (state.Message, error)
```

```go
package httpapi

// NewHandler adapts a *yasdb.DB to the full Durable Streams HTTP surface —
// literally today's internal/ds.Server.ServeHTTP, moved to sit on top of
// the new core instead of being the only way to reach it. An embedder who
// wants zero HTTP never imports this package.
func NewHandler(db *yasdb.DB) http.Handler
```

**Deliberately excluded from the core surface, HTTP-only via `httpapi`:**
bulk-provision (`bulkprovision.go` — explicitly a load-test setup tool),
`/__admin/streams` listing, Prometheus metrics. These are operational
surfaces, not things an embedder programmatically appends/reads through —
keeping them out of `yasdb` is part of what keeps it small.

## Lifecycle & concurrency notes

- `context.Context` per call (not one at `Open` time) so `Tail`/`TailState`
  compose with a caller's own request lifecycle — this is the one place
  the current design's `Config.LongPollTimeout` (an HTTP-shaped idea, "how
  long before returning `204`") doesn't map cleanly onto a Go API:
  cancellation via `ctx.Done()` is the natural replacement, and callers who
  want a timeout use `context.WithTimeout` the normal way. `LiveCursor.Next`
  blocking until `ctx.Done()` (instead of returning a synthetic "204
  timeout, poll again" the way the protocol's long-poll does) is a real
  behavior difference from the wire protocol, and worth flagging explicitly
  rather than silently picking one.
- Errors: today's `appendResp`/`createResult`/etc. carry an HTTP status
  code (`204`, `403`, `409`, ...) as their primary signal. The public API
  needs real `error` values (e.g. `ErrClosed`, `ErrProducerConflict`,
  `ErrSeqGap`, wrapping enough detail — epoch/seq — to match what the
  protocol's headers carry today) instead of a caller switching on an int.
  This mapping needs to be enumerated once, carefully, against every status
  code `handlers.go` currently writes — not guessed at per-call-site.

## Migration plan (independently shippable phases)

1. **`Stream.Append`/`Create`/`Close`/`Delete`** — thin wraps over
   `submitAppend`/`createStream`/`deleteStreamLocked`. Lowest risk: these
   internal methods are already exercised by the full existing test suite
   through the HTTP handlers: 332/332 passing conformance tests, Porcupine
   linearizability, Maelstrom. The wrap doesn't change what runs underneath.
2. **`Stream.Read`/`Cursor`** — wraps `readRange`, same reasoning.
3. **`Stream.Tail`/`LiveCursor`** — wraps `liveState` + `waiterChan`; this
   is where the long-poll-timeout-vs-context-cancellation question above
   needs an actual decision before writing code.
4. **`Stream.Materialize`/`TailState`** — built on phase 2/3 plus
   `internal/state`, replacing `state.go`'s self-request. This phase also
   *simplifies* existing code (deletes the hand-rolled `http.ResponseWriter`
   recorder), not just adds new surface.
5. **`httpapi.NewHandler`** — move `ServeHTTP` and friends to sit on the
   new core. `main.go` switches to `yasdb.Open` + `httpapi.NewHandler`;
   behavior must stay byte-identical (same conformance/Porcupine/Maelstrom
   suites as the regression gate).
6. **Fork** — wraps `createFork`, same pattern as phase 1.

Each phase is a self-contained, reviewable, revertable change against the
existing test suite — no phase requires the others to be useful on its own
(phase 1 alone is already a real embeddable append-only-log library).

## Open questions

- **Package name**: `yasdb` (importer writes `yasdb.Open`) vs. something
  under `internal/`'s sibling like `streams`/`embed`. Cosmetic, but the
  import path is a public commitment once released.
- **`Tail`/`TailState` shape**: a pull-style `Cursor.Next(ctx)` (proposed
  above, mirrors `Read`) vs. a Go channel (`<-chan Record`) more idiomatic
  for "subscribe and range over it," but harder to cleanly cancel/back-
  pressure than a pull call. Leaning pull-style for symmetry with `Read`
  and explicit backpressure, but this is worth deciding deliberately.
- **Versioning/stability**: once `yasdb.Open` ships, its signature is a
  real compatibility promise in a way `internal/ds.NewServer` never was.
  Worth deciding up front whether this starts pre-1.0 (`v0.x`, breaking
  changes allowed) given the project's own version is currently v0.0.1.
