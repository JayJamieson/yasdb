package state

import "testing"

func TestValidateChangeMessage(t *testing.T) {
	cases := []struct {
		name    string
		msg     Message
		wantErr bool
	}{
		{"valid insert", Message{Type: "user", Key: "1", Value: []byte(`{"name":"Alice"}`), Headers: Headers{Operation: OpInsert}}, false},
		{"valid update", Message{Type: "user", Key: "1", Value: []byte(`{"name":"Bob"}`), Headers: Headers{Operation: OpUpdate}}, false},
		{"valid delete no value", Message{Type: "user", Key: "1", Headers: Headers{Operation: OpDelete}}, false},
		{"valid delete null value", Message{Type: "user", Key: "1", Value: []byte(`null`), Headers: Headers{Operation: OpDelete}}, false},
		{"missing type", Message{Key: "1", Value: []byte(`{}`), Headers: Headers{Operation: OpInsert}}, true},
		{"missing key", Message{Type: "user", Value: []byte(`{}`), Headers: Headers{Operation: OpInsert}}, true},
		{"insert missing value", Message{Type: "user", Key: "1", Headers: Headers{Operation: OpInsert}}, true},
		{"update missing value", Message{Type: "user", Key: "1", Headers: Headers{Operation: OpUpdate}}, true},
		{"bad operation", Message{Type: "user", Key: "1", Value: []byte(`{}`), Headers: Headers{Operation: "bogus"}}, true},
		{"missing operation", Message{Type: "user", Key: "1", Value: []byte(`{}`)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.msg.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateControlMessage(t *testing.T) {
	cases := []struct {
		name    string
		msg     Message
		wantErr bool
	}{
		{"snapshot-start", Message{Headers: Headers{Control: ControlSnapshotStart}}, false},
		{"snapshot-end", Message{Headers: Headers{Control: ControlSnapshotEnd, Offset: "123_0"}}, false},
		{"reset", Message{Headers: Headers{Control: ControlReset}}, false},
		{"bad control", Message{Headers: Headers{Control: "bogus"}}, true},
		{"both operation and control set", Message{Type: "user", Key: "1", Headers: Headers{Operation: OpInsert, Control: ControlReset}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.msg.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestIsControl(t *testing.T) {
	if (Message{Type: "user", Key: "1", Headers: Headers{Operation: OpInsert}}).IsControl() {
		t.Fatal("change message reported as control")
	}
	if !(Message{Headers: Headers{Control: ControlReset}}).IsControl() {
		t.Fatal("control message not reported as control")
	}
}
