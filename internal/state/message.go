// Package state implements the Durable Streams State Protocol
// (https://github.com/durable-streams/durable-streams/blob/main/packages/state/STATE-PROTOCOL.md),
// an extension of the base Durable Streams Protocol that gives JSON stream
// messages semantic meaning: change messages (insert/update/delete) and
// control messages (snapshot boundaries, reset), materialized into
// type/key-addressed entity state.
//
// The protocol is transport-agnostic: it operates on ordinary
// Content-Type: application/json streams via the base protocol's
// append/read operations. So no changes to the underlying stream engine
// are needed to support it. This package provides the message types,
// validation, and a materializer for applying a message sequence to build
// up state.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Operation is the kind of mutation a change message applies (§4.1).
type Operation string

const (
	OpInsert Operation = "insert"
	OpUpdate Operation = "update"
	OpDelete Operation = "delete"
)

// Control is a stream-management signal carried by a control message (§4.2).
type Control string

const (
	ControlSnapshotStart Control = "snapshot-start"
	ControlSnapshotEnd   Control = "snapshot-end"
	ControlReset         Control = "reset"
)

// Headers carries operation metadata for change messages, or the control
// signal for control messages (§5.1, §5.2). The two message kinds are
// discriminated by which of Operation/Control is set.
type Headers struct {
	// Operation is set on change messages: one of insert/update/delete.
	Operation Operation `json:"operation,omitempty"`
	// Control is set on control messages: one of snapshot-start/snapshot-end/reset.
	Control Control `json:"control,omitempty"`
	// Txid is an opaque, optional transaction identifier grouping related changes.
	Txid string `json:"txid,omitempty"`
	// Timestamp is an optional RFC 3339 timestamp of when the change occurred.
	Timestamp string `json:"timestamp,omitempty"`
	// Offset is an optional stream offset associated with a control event.
	Offset string `json:"offset,omitempty"`
}

// Message is a single State Protocol message: either a change message
// (Type/Key/Value set, Headers.Operation set) or a control message
// (only Headers.Control set).
type Message struct {
	Type     string          `json:"type,omitempty"`
	Key      string          `json:"key,omitempty"`
	Value    json.RawMessage `json:"value,omitempty"`
	OldValue json.RawMessage `json:"old_value,omitempty"`
	Headers  Headers         `json:"headers"`
}

// IsControl reports whether m is a control message rather than a change message.
func (m Message) IsControl() bool {
	return m.Headers.Control != ""
}

var (
	errMissingType      = errors.New("state: change message missing type")
	errMissingKey       = errors.New("state: change message missing key")
	errBadOperation     = errors.New("state: headers.operation must be insert, update, or delete")
	errMissingValue     = errors.New("state: insert/update message missing value")
	errBadControl       = errors.New("state: headers.control must be snapshot-start, snapshot-end, or reset")
	errAmbiguousHeaders = errors.New("state: headers must not set both operation and control")
)

// Validate checks m against the field requirements of §5.1 (change messages)
// and §5.2 (control messages).
func (m Message) Validate() error {
	if m.Headers.Operation != "" && m.Headers.Control != "" {
		return errAmbiguousHeaders
	}
	if m.IsControl() {
		switch m.Headers.Control {
		case ControlSnapshotStart, ControlSnapshotEnd, ControlReset:
		default:
			return fmt.Errorf("%w: got %q", errBadControl, m.Headers.Control)
		}
		return nil
	}
	if m.Type == "" {
		return errMissingType
	}
	if m.Key == "" {
		return errMissingKey
	}
	switch m.Headers.Operation {
	case OpInsert, OpUpdate:
		if len(m.Value) == 0 {
			return fmt.Errorf("%w for %q operation", errMissingValue, m.Headers.Operation)
		}
	case OpDelete:
		// value is optional for delete.
	default:
		return fmt.Errorf("%w: got %q", errBadOperation, m.Headers.Operation)
	}
	return nil
}
