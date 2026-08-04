package state

import (
	"encoding/json"
	"reflect"
	"testing"
)

func rawJSON(t *testing.T, v string) json.RawMessage {
	t.Helper()
	if !json.Valid([]byte(v)) {
		t.Fatalf("invalid JSON literal in test: %s", v)
	}
	return json.RawMessage(v)
}

// TestSpecExample reproduces the worked example in STATE-PROTOCOL.md §6
// verbatim: two inserts and an update on the same key.
func TestSpecExample(t *testing.T) {
	msgs := []Message{
		{Type: "user", Key: "1", Value: rawJSON(t, `{"name":"Alice"}`), Headers: Headers{Operation: OpInsert}},
		{Type: "user", Key: "2", Value: rawJSON(t, `{"name":"Bob"}`), Headers: Headers{Operation: OpInsert}},
		{Type: "user", Key: "1", Value: rawJSON(t, `{"name":"Alice Smith"}`), Headers: Headers{Operation: OpUpdate}},
	}
	m := NewMaterializer()
	if errs := m.ApplyAll(msgs); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	snap := m.Snapshot()
	want := map[string]map[string]json.RawMessage{
		"user": {
			"1": rawJSON(t, `{"name":"Alice Smith"}`),
			"2": rawJSON(t, `{"name":"Bob"}`),
		},
	}
	if !reflect.DeepEqual(normalize(snap), normalize(want)) {
		t.Fatalf("materialized state = %s, want %s", dump(snap), dump(want))
	}
}

func TestDeleteRemovesEntity(t *testing.T) {
	m := NewMaterializer()
	apply(t, m, Message{Type: "user", Key: "1", Value: rawJSON(t, `{"name":"Alice"}`), Headers: Headers{Operation: OpInsert}})
	apply(t, m, Message{Type: "user", Key: "1", Headers: Headers{Operation: OpDelete}})

	snap := m.Snapshot()
	if coll, ok := snap["user"]; ok {
		if _, present := coll["1"]; present {
			t.Fatalf("deleted key still present: %v", coll)
		}
	}
}

func TestMultiTypeStream(t *testing.T) {
	m := NewMaterializer()
	apply(t, m, Message{Type: "user", Key: "user:123", Value: rawJSON(t, `{"name":"Alice"}`), Headers: Headers{Operation: OpInsert}})
	apply(t, m, Message{Type: "message", Key: "msg:456", Value: rawJSON(t, `{"userId":"user:123","text":"Hello!"}`), Headers: Headers{Operation: OpInsert}})
	apply(t, m, Message{Type: "reaction", Key: "reaction:789", Value: rawJSON(t, `{"messageId":"msg:456","emoji":"👍"}`), Headers: Headers{Operation: OpInsert}})

	snap := m.Snapshot()
	for _, typ := range []string{"user", "message", "reaction"} {
		if len(snap[typ]) != 1 {
			t.Fatalf("type %q: got %d entities, want 1", typ, len(snap[typ]))
		}
	}
}

func TestResetClearsState(t *testing.T) {
	m := NewMaterializer()
	apply(t, m, Message{Type: "user", Key: "1", Value: rawJSON(t, `{"name":"Alice"}`), Headers: Headers{Operation: OpInsert}})
	apply(t, m, Message{Headers: Headers{Control: ControlReset}})

	snap := m.Snapshot()
	if len(snap) != 0 {
		t.Fatalf("state not cleared after reset: %v", snap)
	}
}

func TestSnapshotBoundaryTracking(t *testing.T) {
	m := NewMaterializer()
	if m.InSnapshot() {
		t.Fatal("InSnapshot() true before any snapshot-start")
	}
	apply(t, m, Message{Headers: Headers{Control: ControlSnapshotStart, Offset: "0_0"}})
	if !m.InSnapshot() {
		t.Fatal("InSnapshot() false after snapshot-start")
	}
	apply(t, m, Message{Type: "user", Key: "1", Value: rawJSON(t, `{"name":"Alice"}`), Headers: Headers{Operation: OpInsert}})
	apply(t, m, Message{Headers: Headers{Control: ControlSnapshotEnd, Offset: "1_0"}})
	if m.InSnapshot() {
		t.Fatal("InSnapshot() true after snapshot-end")
	}
	if len(m.Snapshot()["user"]) != 1 {
		t.Fatal("entity applied during snapshot boundary not materialized")
	}
}

// TestInvalidMessageDoesNotCorruptRest checks that a malformed message is
// rejected without disturbing state from valid messages already applied.
func TestInvalidMessageDoesNotCorruptRest(t *testing.T) {
	m := NewMaterializer()
	apply(t, m, Message{Type: "user", Key: "1", Value: rawJSON(t, `{"name":"Alice"}`), Headers: Headers{Operation: OpInsert}})

	errs := m.ApplyAll([]Message{
		{Type: "user", Key: "2", Headers: Headers{Operation: OpInsert}}, // missing value: invalid
		{Type: "user", Key: "3", Value: rawJSON(t, `{"name":"Carol"}`), Headers: Headers{Operation: OpInsert}},
	})
	if len(errs) != 1 {
		t.Fatalf("got %d errors, want 1: %v", len(errs), errs)
	}
	snap := m.Snapshot()
	if _, ok := snap["user"]["2"]; ok {
		t.Fatal("invalid message was applied despite validation failure")
	}
	if _, ok := snap["user"]["1"]; !ok {
		t.Fatal("prior valid state lost after a later invalid message")
	}
	if _, ok := snap["user"]["3"]; !ok {
		t.Fatal("valid message after an invalid one was not applied")
	}
}

func apply(t *testing.T, m *Materializer, msg Message) {
	t.Helper()
	if err := m.Apply(msg); err != nil {
		t.Fatalf("Apply(%+v) unexpected error: %v", msg, err)
	}
}

// normalize re-marshals raw JSON values through json.Marshal/Unmarshal so
// byte-level formatting differences (spacing) don't cause false mismatches in
// reflect.DeepEqual comparisons.
func normalize(in map[string]map[string]json.RawMessage) map[string]map[string]any {
	out := make(map[string]map[string]any, len(in))
	for typ, coll := range in {
		c := make(map[string]any, len(coll))
		for k, v := range coll {
			var parsed any
			if err := json.Unmarshal(v, &parsed); err != nil {
				parsed = string(v)
			}
			c[k] = parsed
		}
		out[typ] = c
	}
	return out
}

func dump(in map[string]map[string]json.RawMessage) string {
	b, _ := json.Marshal(in)
	return string(b)
}
