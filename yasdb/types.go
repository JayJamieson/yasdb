package yasdb

import "github.com/JayJamieson/yasdb/internal/ds"

// Offset is a position within a stream: the pair (record sequence number,
// byte offset into that record — nonzero only mid-way through a record
// that exceeded a single read's byte limit; always 0 for JSON streams).
// The zero value is the start of the stream.
type Offset = ds.Offset

// StreamInfo is a stream's current metadata and live state — the
// in-process equivalent of an HTTP HEAD response.
type StreamInfo = ds.StreamInfo

// Cursor is a bounded catch-up scan over a stream, from Stream.Read.
type Cursor = ds.Cursor

// LiveCursor is a live, blocking read over a stream, from Stream.Tail. An
// Append to the stream — in-process or over an httpapi.Handler — wakes a
// parked LiveCursor.Next the same way it wakes an HTTP long-poll/SSE
// reader; see internal/ds/public.go's LiveCursor doc for the mechanism.
type LiveCursor = ds.LiveCursor

// StateCursor is a live, one-message-at-a-time read of a State Protocol
// stream, from Stream.TailState.
type StateCursor = ds.StateCursor
