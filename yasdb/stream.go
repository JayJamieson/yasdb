package yasdb

import (
	"context"
	"encoding/json"

	"github.com/JayJamieson/yasdb/internal/state"
)

// Stream is a cheap handle for one stream path — like bbolt's Bucket,
// obtaining one does no I/O. Get one from DB.Stream.
type Stream struct {
	db   *DB
	path string
}

// Path returns the stream's path.
func (s *Stream) Path() string { return s.path }

// Create creates the stream with the given content type (no initial
// body). It is idempotent: calling it again with the same content type
// succeeds; a different content type on an existing stream returns
// ErrConflict.
func (s *Stream) Create(ctx context.Context, contentType string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := s.db.srv.CreateStream(s.path, contentType)
	return err
}

// Fork creates path as a new stream forked from s, diverging at offset at.
// The fork inherits s's content type. Pass a StreamInfo.Tail (from Info)
// as at to fork from the current end of the stream; the zero Offset forks
// from the very beginning.
func (s *Stream) Fork(ctx context.Context, path string, at Offset) (*Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := s.db.srv.CreateFork(path, s.path, at); err != nil {
		return nil, err
	}
	return s.db.Stream(path), nil
}

// Append appends records to the stream, in order, as one durable batch,
// and returns the offset immediately after the last one written.
func (s *Stream) Append(ctx context.Context, records ...[]byte) (Offset, error) {
	if err := ctx.Err(); err != nil {
		return Offset{}, err
	}
	return s.db.srv.Append(s.path, records)
}

// AppendJSON marshals each v and appends them as records, in order.
func (s *Stream) AppendJSON(ctx context.Context, v ...any) (Offset, error) {
	if err := ctx.Err(); err != nil {
		return Offset{}, err
	}
	records := make([][]byte, len(v))
	for i, val := range v {
		b, err := json.Marshal(val)
		if err != nil {
			return Offset{}, err
		}
		records[i] = b
	}
	return s.db.srv.Append(s.path, records)
}

// Close marks the stream closed (EOF). Idempotent.
func (s *Stream) Close(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := s.db.srv.CloseStream(s.path)
	return err
}

// Delete deletes the stream: hard-deleted if nothing forks it, otherwise
// soft-deleted (data retained for fork readers; the path itself then
// returns ErrGone).
func (s *Stream) Delete(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.srv.DeleteStream(s.path)
}

// Info returns the stream's current metadata and live state.
func (s *Stream) Info(ctx context.Context) (StreamInfo, error) {
	if err := ctx.Err(); err != nil {
		return StreamInfo{}, err
	}
	return s.db.srv.Info(s.path)
}

// Read returns a bounded catch-up Cursor starting at from. Read itself
// does no I/O; state is resolved on the Cursor's first Next call.
func (s *Stream) Read(ctx context.Context, from Offset) (*Cursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.db.srv.Read(s.path, from)
}

// Tail returns a live, blocking Cursor starting at from. Its Next call
// parks until data commits, the stream is deleted, or ctx is done —
// there is no synthetic timeout response the way the wire protocol's
// long-poll has; use context.WithTimeout for that.
func (s *Stream) Tail(ctx context.Context, from Offset) (*LiveCursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.db.srv.Tail(s.path, from)
}

// Materialize replays the stream's full history through a fresh
// state.Materializer (github.com/JayJamieson/yasdb/internal/state) and
// returns it, plus the offset to resume live-tailing from via TailState.
// Returns ErrNotStateProtocolStream if the stream is not application/json
// State Protocol messages.
//
// The returned *state.Materializer's type lives in an internal/ package,
// so it cannot be named in code outside this module — but its exported
// methods (Snapshot, Apply, InSnapshot, Reset) are callable normally on
// the value you get back; you just can't declare a variable of that type
// yourself.
func (s *Stream) Materialize(ctx context.Context) (*state.Materializer, Offset, error) {
	return s.db.srv.Materialize(ctx, s.path)
}

// TailState returns a live, one-message-at-a-time StateCursor starting at
// from (see Materialize's returned offset to resume from the current
// materialized state).
func (s *Stream) TailState(ctx context.Context, from Offset) (*StateCursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.db.srv.TailState(s.path, from)
}
