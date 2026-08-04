package state

import (
	"encoding/json"
	"sync"
)

// Materializer applies a sequence of State Protocol messages (§6) to build up
// entity state keyed by type then key. It is safe for concurrent use.
type Materializer struct {
	mu         sync.RWMutex
	types      map[string]map[string]json.RawMessage
	inSnapshot bool
}

// NewMaterializer returns an empty Materializer.
func NewMaterializer() *Materializer {
	return &Materializer{types: make(map[string]map[string]json.RawMessage)}
}

// Apply validates and applies a single message. Change messages upsert
// (insert/update) or remove (delete) the entity at Type/Key. Control
// messages are handled per §4.2: reset clears all materialized state;
// snapshot-start and snapshot-end are tracked (InSnapshot), but otherwise
// left to application logic.
//
// An invalid message is rejected (not applied), and its error is
// returned, so one malformed message in a stream cannot corrupt
// materialization of the rest.
func (m *Materializer) Apply(msg Message) error {
	if err := msg.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if msg.IsControl() {
		switch msg.Headers.Control {
		case ControlReset:
			m.types = make(map[string]map[string]json.RawMessage)
			m.inSnapshot = false
		case ControlSnapshotStart:
			m.inSnapshot = true
		case ControlSnapshotEnd:
			m.inSnapshot = false
		}
		return nil
	}
	coll := m.types[msg.Type]
	if coll == nil {
		coll = make(map[string]json.RawMessage)
		m.types[msg.Type] = coll
	}
	switch msg.Headers.Operation {
	case OpInsert, OpUpdate:
		coll[msg.Key] = append(json.RawMessage(nil), msg.Value...)
	case OpDelete:
		delete(coll, msg.Key)
	}
	return nil
}

// ApplyAll applies each message in order, collecting (not stopping on) any
// per-message validation errors so one bad message doesn't block the rest.
func (m *Materializer) ApplyAll(msgs []Message) []error {
	var errs []error
	for _, msg := range msgs {
		if err := m.Apply(msg); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// InSnapshot reports whether the materializer is currently between a
// snapshot-start and snapshot-end control message.
func (m *Materializer) InSnapshot() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.inSnapshot
}

// Reset clears all materialized state, as if a reset control message had
// been applied.
func (m *Materializer) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.types = make(map[string]map[string]json.RawMessage)
	m.inSnapshot = false
}

// Snapshot returns a deep-copied, point-in-time view of materialized state,
// keyed by entity type then key.
func (m *Materializer) Snapshot() map[string]map[string]json.RawMessage {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]map[string]json.RawMessage, len(m.types))
	for typ, coll := range m.types {
		c := make(map[string]json.RawMessage, len(coll))
		for k, v := range coll {
			c[k] = append(json.RawMessage(nil), v...)
		}
		out[typ] = c
	}
	return out
}
