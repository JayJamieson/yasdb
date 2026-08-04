package ds

import (
	"errors"
	"strconv"
)

// Offset is a position within a stream. The wire format matches the
// reference implementations: `<seq>_<byteOffset>`, both
// 16-digit zero-padded decimals, e.g. "0000000000000042_0000000000000000".
//
//   - Seq is the next RecordData sequence number to read.
//   - Byte is how many bytes of that record have already been delivered.
//     It is non-zero only when a single record exceeded a read's byte
//     limit. It is always 0 for application/json streams.
type Offset struct {
	Seq  uint64
	Byte uint64
}

const offsetTokenLen = 33 // 16 + '_' + 16

var errBadOffset = errors.New("malformed offset")

// String renders the offset in its fixed-width wire form.
func (o Offset) String() string {
	return string(appendOffset(make([]byte, 0, offsetTokenLen), o))
}

// appendOffset appends the fixed-width wire form (<16-digit seq>_<16-digit
// byte>) to dst. It is hand-rolled to avoid fmt reflection and allocation on
// the hot live-read control-frame path.
func appendOffset(dst []byte, o Offset) []byte {
	dst = appendPad16(dst, o.Seq)
	dst = append(dst, '_')
	return appendPad16(dst, o.Byte)
}

// appendPad16 appends v as exactly 16 zero-padded decimal digits.
func appendPad16(dst []byte, v uint64) []byte {
	var tmp [16]byte
	for i := 15; i >= 0; i-- {
		tmp[i] = byte('0' + v%10)
		v /= 10
	}
	return append(dst, tmp[:]...)
}

// tailOffset returns the offset for the position immediately after seq nextSeq.
func tailOffset(nextSeq uint64) Offset { return Offset{Seq: nextSeq, Byte: 0} }

// parsedOffset is the result of interpreting a client-supplied offset token.
type parsedOffset struct {
	off   Offset
	isNow bool // the `now` sentinel: read from the current tail
}

// parseOffset interprets an offset query value.
//
//   - "" (absent) and "-1" both mean the beginning of the stream -> {0,0}.
//   - "now" -> the current tail (isNow=true); the caller resolves it.
//   - a real 33-char token -> the encoded position.
//
// Any other value is malformed and yields errBadOffset (HTTP 400). The
// sentinels "-1" and "now" can never collide with minted offsets: minted
// offsets are always 33 characters of digits and one underscore.
func parseOffset(raw string) (parsedOffset, error) {
	switch raw {
	case "", "-1":
		return parsedOffset{off: Offset{0, 0}}, nil
	case "now":
		return parsedOffset{isNow: true}, nil
	}

	if len(raw) != offsetTokenLen || raw[16] != '_' {
		return parsedOffset{}, errBadOffset
	}
	seqStr, byteStr := raw[:16], raw[17:]
	seq, err := strconv.ParseUint(seqStr, 10, 64)
	if err != nil {
		return parsedOffset{}, errBadOffset
	}
	boff, err := strconv.ParseUint(byteStr, 10, 64)
	if err != nil {
		return parsedOffset{}, errBadOffset
	}
	return parsedOffset{off: Offset{Seq: seq, Byte: boff}}, nil
}
