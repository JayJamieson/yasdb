package yasdb

import "github.com/JayJamieson/yasdb/internal/ds"

// Sentinel errors returned by Stream/DB methods. Check with errors.Is.
// These are aliases of internal/ds's sentinels (same *errors.errorString
// value), not copies, so errors.Is works whether an error crossed the
// internal/ds boundary or not.
var (
	// ErrNotFound means the stream was never created, or was hard-deleted.
	ErrNotFound = ds.ErrNotFound
	// ErrGone means the stream was soft-deleted: it is still referenced by
	// a fork, so its data lives on, but its own path no longer answers.
	ErrGone = ds.ErrGone
	// ErrConflict means a Create/Fork call named an existing path with a
	// different configuration than what is already there.
	ErrConflict = ds.ErrConflict
	// ErrStreamClosed means an Append targeted an already-closed stream.
	ErrStreamClosed = ds.ErrStreamClosed
	// ErrBadRequest means a call's parameters were rejected outright.
	ErrBadRequest = ds.ErrBadRequest
	// ErrNotStateProtocolStream means Materialize/TailState found a stream
	// whose body is not a JSON State Protocol message array.
	ErrNotStateProtocolStream = ds.ErrNotStateProtocolStream
)
