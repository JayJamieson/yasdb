package ds

import (
	"bytes"
	"encoding/json"
	"errors"
)

const contentTypeJSON = "application/json"

var (
	errInvalidJSON = errors.New("invalid JSON")
	errEmptyArray  = errors.New("empty JSON array")
)

// flattenJSON implements JSON mode array flattening (protocol §9.1.2). It parses
// an append body, flattening exactly one level of a top-level array so each
// element becomes its own message. A non-array body is a single message. Every
// message is validated and stored compacted.
//
// An empty top-level array yields errEmptyArray (the caller returns 400 for
// appends, per §9.1.3).
func flattenJSON(body []byte) ([][]byte, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, errInvalidJSON
	}
	if trimmed[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, errInvalidJSON
		}
		if len(arr) == 0 {
			return nil, errEmptyArray
		}
		out := make([][]byte, len(arr))
		for i, el := range arr {
			c, err := compactJSON(el)
			if err != nil {
				return nil, errInvalidJSON
			}
			out[i] = c
		}
		return out, nil
	}
	if !json.Valid(trimmed) {
		return nil, errInvalidJSON
	}
	c, err := compactJSON(trimmed)
	if err != nil {
		return nil, errInvalidJSON
	}
	return [][]byte{c}, nil
}

func compactJSON(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// wrapJSONArray joins stored JSON messages into a single array body.
func wrapJSONArray(records [][]byte) []byte {
	return appendJSONArray(make([]byte, 0, 2), records)
}

// appendJSONArray appends the single-array body form of records to dst, letting
// callers reuse a scratch buffer instead of allocating one per read.
func appendJSONArray(dst []byte, records [][]byte) []byte {
	dst = append(dst, '[')
	for i, r := range records {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, r...)
	}
	return append(dst, ']')
}
