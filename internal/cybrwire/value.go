// Package cybrwire is the Go side of Core's "cybr value" wire codec: the byte
// format Core uses for mutation payloads (write path) and query row payloads
// (read path). It is a faithful port of Core's Rust contract, defined in
// calystral-io/core:
//
//   - the value codec       core/src/proc/wire.rs   (this file)
//   - the mutation framing  core/src/mutate.rs      (mutation.go)
//
// Core owns the contract; this package must encode/decode byte-for-byte the
// same, so a mutation the BFF encodes decodes on Core and a row Core encodes
// decodes here. The tests pin that with golden bytes computed from the layout.
//
// # The encodable subset
//
// Only the data values that can cross the boundary are representable: Bool, Int,
// Dec, Str, Blob, Time, Date, Dur, Json, and Array of those. Core's runtime-only
// cvm Value kinds (anchors, pages, iterators, records, ...) are not on the wire
// and have no Go representation here; encountering one on decode is a typed
// error (an unknown tag), never a panic.
//
// # Determinism
//
// The encoding is canonical: a fixed one-byte tag per kind, little-endian
// fixed-width integers, and u32-length-prefixed bytes/strings. Equal values
// encode to identical bytes (what lets Calvin replay a mutation byte-for-byte).
// Decoding is total and bounded: every malformed input is a typed error, an
// untrusted length never pre-allocates, and array nesting is capped at MaxDepth.
package cybrwire

import (
	"encoding/binary"
	"fmt"
	"unicode/utf8"
)

// MaxDepth is the maximum array-nesting depth the codec encodes or decodes. It
// exists only so a hostile or corrupt input cannot drive unbounded recursion;
// a real data value is shallow. Matches core/src/proc/wire.rs MAX_DEPTH.
const MaxDepth = 32

// Kind is the one-byte discriminant prefixing each encoded value. These numbers
// are a wire contract (stable, append-only) shared with core/src/proc/wire.rs.
type Kind uint8

// The value tags. Kept identical to Core's `tag` module.
const (
	KindBool  Kind = 0x01
	KindInt   Kind = 0x02
	KindDec   Kind = 0x03
	KindStr   Kind = 0x04
	KindBlob  Kind = 0x05
	KindTime  Kind = 0x06
	KindDate  Kind = 0x07
	KindDur   Kind = 0x08
	KindJSON  Kind = 0x09
	KindArray Kind = 0x0A
)

// Value is one encodable cybr value. It is a tagged union: exactly the field(s)
// selected by kind carry meaning. Construct one with the typed constructors
// (Bool, Int, ...) rather than by hand so kind and payload stay consistent.
type Value struct {
	kind  Kind
	i     int64   // Int, Time, Dur
	date  int32   // Date
	b     bool    // Bool
	str   string  // Dec, Str
	raw   []byte  // Blob, Json
	array []Value // Array
}

// Kind reports which value this is.
func (v Value) Kind() Kind { return v.kind }

// Bool returns a boolean value.
func Bool(b bool) Value { return Value{kind: KindBool, b: b} }

// Int returns a 64-bit signed integer value.
func Int(i int64) Value { return Value{kind: KindInt, i: i} }

// Dec returns a decimal value carried as its canonical decimal string.
func Dec(s string) Value { return Value{kind: KindDec, str: s} }

// Str returns a UTF-8 string value.
func Str(s string) Value { return Value{kind: KindStr, str: s} }

// Blob returns an opaque byte-string value. The bytes are retained, not copied.
func Blob(b []byte) Value { return Value{kind: KindBlob, raw: b} }

// Time returns a timestamp value (cvm's i64 time representation).
func Time(t int64) Value { return Value{kind: KindTime, i: t} }

// Date returns a date value (cvm's i32 day representation).
func Date(d int32) Value { return Value{kind: KindDate, date: d} }

// Dur returns a duration value (cvm's i64 duration representation).
func Dur(d int64) Value { return Value{kind: KindDur, i: d} }

// JSON returns a JSON value carried as cvm's canonical binary json bytes (not
// text). The bytes are retained, not copied.
func JSON(b []byte) Value { return Value{kind: KindJSON, raw: b} }

// Array returns an array value over the given elements.
func Array(elems []Value) Value { return Value{kind: KindArray, array: elems} }

// AsBool reports the value's boolean; the second result is false unless the
// value is a Bool. The typed accessors mirror the constructors and are how a
// caller reads a decoded value back out.
func (v Value) AsBool() (bool, bool) { return v.b, v.kind == KindBool }

// AsInt reports the value's int64 for Int/Time/Dur kinds.
func (v Value) AsInt() (int64, bool) {
	return v.i, v.kind == KindInt || v.kind == KindTime || v.kind == KindDur
}

// AsDate reports the value's int32 for a Date.
func (v Value) AsDate() (int32, bool) { return v.date, v.kind == KindDate }

// AsString reports the value's string for Dec/Str kinds.
func (v Value) AsString() (string, bool) {
	return v.str, v.kind == KindDec || v.kind == KindStr
}

// AsBytes reports the value's bytes for Blob/Json kinds.
func (v Value) AsBytes() ([]byte, bool) {
	return v.raw, v.kind == KindBlob || v.kind == KindJSON
}

// AsArray reports the value's elements for an Array.
func (v Value) AsArray() ([]Value, bool) { return v.array, v.kind == KindArray }

// EncodeValue encodes one value to its self-describing wire form.
func EncodeValue(v Value) ([]byte, error) {
	var out []byte
	out, err := writeValue(out, v, 0)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DecodeValue decodes one value from a complete wire form, requiring every byte
// to be consumed (trailing bytes are an error).
func DecodeValue(b []byte) (Value, error) {
	cur := &cursor{buf: b}
	v, err := readValue(cur, 0)
	if err != nil {
		return Value{}, err
	}
	if !cur.empty() {
		return Value{}, &ValueError{Code: ErrTrailingBytes}
	}
	return v, nil
}

func writeValue(out []byte, v Value, depth int) ([]byte, error) {
	if depth > MaxDepth {
		return nil, &ValueError{Code: ErrDepthExceeded}
	}
	switch v.kind {
	case KindBool:
		out = append(out, byte(KindBool))
		if v.b {
			out = append(out, 1)
		} else {
			out = append(out, 0)
		}
	case KindInt:
		out = append(out, byte(KindInt))
		out = appendI64(out, v.i)
	case KindDec:
		out = append(out, byte(KindDec))
		var err error
		if out, err = appendLenPrefixed(out, []byte(v.str)); err != nil {
			return nil, err
		}
	case KindStr:
		out = append(out, byte(KindStr))
		var err error
		if out, err = appendLenPrefixed(out, []byte(v.str)); err != nil {
			return nil, err
		}
	case KindBlob:
		out = append(out, byte(KindBlob))
		var err error
		if out, err = appendLenPrefixed(out, v.raw); err != nil {
			return nil, err
		}
	case KindTime:
		out = append(out, byte(KindTime))
		out = appendI64(out, v.i)
	case KindDate:
		out = append(out, byte(KindDate))
		out = appendI32(out, v.date)
	case KindDur:
		out = append(out, byte(KindDur))
		out = appendI64(out, v.i)
	case KindJSON:
		out = append(out, byte(KindJSON))
		var err error
		if out, err = appendLenPrefixed(out, v.raw); err != nil {
			return nil, err
		}
	case KindArray:
		out = append(out, byte(KindArray))
		n, err := u32Len(len(v.array))
		if err != nil {
			return nil, err
		}
		out = binary.LittleEndian.AppendUint32(out, n)
		for _, elem := range v.array {
			if out, err = writeValue(out, elem, depth+1); err != nil {
				return nil, err
			}
		}
	default:
		// A Value built through the constructors always has a known kind; a zero
		// Value (kind 0) or a hand-forged one lands here.
		return nil, &ValueError{Code: ErrUnknownTag, Tag: byte(v.kind)}
	}
	return out, nil
}

func readValue(cur *cursor, depth int) (Value, error) {
	if depth > MaxDepth {
		return Value{}, &ValueError{Code: ErrDepthExceeded}
	}
	tag, err := cur.u8()
	if err != nil {
		return Value{}, err
	}
	switch Kind(tag) {
	case KindBool:
		b, err := cur.u8()
		if err != nil {
			return Value{}, err
		}
		switch b {
		case 0:
			return Bool(false), nil
		case 1:
			return Bool(true), nil
		default:
			return Value{}, &ValueError{Code: ErrInvalidBool, Tag: b}
		}
	case KindInt:
		i, err := cur.i64()
		if err != nil {
			return Value{}, err
		}
		return Int(i), nil
	case KindDec:
		s, err := cur.str()
		if err != nil {
			return Value{}, err
		}
		return Dec(s), nil
	case KindStr:
		s, err := cur.str()
		if err != nil {
			return Value{}, err
		}
		return Str(s), nil
	case KindBlob:
		raw, err := cur.bytes()
		if err != nil {
			return Value{}, err
		}
		return Blob(raw), nil
	case KindTime:
		t, err := cur.i64()
		if err != nil {
			return Value{}, err
		}
		return Time(t), nil
	case KindDate:
		d, err := cur.i32()
		if err != nil {
			return Value{}, err
		}
		return Date(d), nil
	case KindDur:
		d, err := cur.i64()
		if err != nil {
			return Value{}, err
		}
		return Dur(d), nil
	case KindJSON:
		raw, err := cur.bytes()
		if err != nil {
			return Value{}, err
		}
		return JSON(raw), nil
	case KindArray:
		count, err := cur.u32()
		if err != nil {
			return Value{}, err
		}
		// Do NOT pre-allocate count elements: it is untrusted, so a truncated
		// body with a huge count must fail by running out of bytes, not by
		// reserving. Each element read bounds-checks.
		var elems []Value
		for i := uint32(0); i < count; i++ {
			elem, err := readValue(cur, depth+1)
			if err != nil {
				return Value{}, err
			}
			elems = append(elems, elem)
		}
		return Array(elems), nil
	default:
		return Value{}, &ValueError{Code: ErrUnknownTag, Tag: tag}
	}
}

// appendLenPrefixed appends a u32 length then the bytes, erroring if the length
// overruns the u32 prefix.
func appendLenPrefixed(out, b []byte) ([]byte, error) {
	n, err := u32Len(len(b))
	if err != nil {
		return nil, err
	}
	out = binary.LittleEndian.AppendUint32(out, n)
	return append(out, b...), nil
}

func appendI64(out []byte, i int64) []byte {
	return binary.LittleEndian.AppendUint64(out, uint64(i))
}

func appendI32(out []byte, i int32) []byte {
	return binary.LittleEndian.AppendUint32(out, uint32(i))
}

// u32Len narrows a Go length to the u32 the wire uses, erroring (TooLarge) if it
// does not fit rather than silently truncating.
func u32Len(n int) (uint32, error) {
	if n < 0 || int64(n) > int64(^uint32(0)) {
		return 0, &ValueError{Code: ErrTooLarge}
	}
	return uint32(n), nil
}

// cursor is a bounds-checked little-endian reader over a byte slice, mirroring
// Core's crate::durable::Cursor for the fields this codec reads.
type cursor struct {
	buf []byte
	pos int
}

func (c *cursor) empty() bool { return c.pos >= len(c.buf) }

// take returns the next n bytes (a sub-slice of buf; the caller must copy if it
// needs to retain them past buf's lifetime) or a Truncated error.
func (c *cursor) take(n int) ([]byte, error) {
	if n < 0 || c.pos+n > len(c.buf) {
		return nil, &ValueError{Code: ErrTruncated}
	}
	b := c.buf[c.pos : c.pos+n]
	c.pos += n
	return b, nil
}

func (c *cursor) u8() (byte, error) {
	b, err := c.take(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (c *cursor) u32() (uint32, error) {
	b, err := c.take(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (c *cursor) u64() (uint64, error) {
	b, err := c.take(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

func (c *cursor) i32() (int32, error) {
	u, err := c.u32()
	return int32(u), err
}

func (c *cursor) i64() (int64, error) {
	u, err := c.u64()
	return int64(u), err
}

// bytes reads a u32-length-prefixed byte run, copied out so it does not alias
// the source buffer.
func (c *cursor) bytes() ([]byte, error) {
	n, err := c.u32()
	if err != nil {
		return nil, err
	}
	b, err := c.take(int(n))
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out, nil
}

// str reads a u32-length-prefixed byte run and requires it to be valid UTF-8,
// matching Core's Cursor::str (used for Dec and Str).
func (c *cursor) str() (string, error) {
	n, err := c.u32()
	if err != nil {
		return "", err
	}
	b, err := c.take(int(n))
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", &ValueError{Code: ErrNonUTF8}
	}
	return string(b), nil
}

// ValueErrorCode enumerates the ways a value fails to encode or decode. It
// mirrors core/src/proc/wire.rs ValueCodecError.
type ValueErrorCode int

// Value codec failure codes.
const (
	// ErrUnsupported: a runtime-only kind that cannot cross the wire. Not
	// reachable through the Go constructors (they only build encodable kinds);
	// kept for parity and for a hand-forged zero Value.
	ErrUnsupported ValueErrorCode = iota
	// ErrTooLarge: a length does not fit the u32 length prefix.
	ErrTooLarge
	// ErrTruncated: the bytes ran out before a field was fully read.
	ErrTruncated
	// ErrUnknownTag: the leading byte is not a known value tag.
	ErrUnknownTag
	// ErrInvalidBool: a boolean byte was neither 0 nor 1.
	ErrInvalidBool
	// ErrNonUTF8: a length-prefixed string was not valid UTF-8.
	ErrNonUTF8
	// ErrDepthExceeded: array nesting exceeded MaxDepth.
	ErrDepthExceeded
	// ErrTrailingBytes: a value decoded cleanly but bytes remained after it.
	ErrTrailingBytes
	// ErrRowNotArray: a query-row payload decoded to a value that is not the
	// top-level array of columns Core's encode_row always emits.
	ErrRowNotArray
	// ErrJSONNotSurfaced: a Json column was decoded but cannot be surfaced as
	// JSON yet — its bytes are cvm canonical BINARY json, not text, so emitting
	// them verbatim would be invalid JSON. Surfacing needs a binary-json decoder
	// (a follow-up); until then a Json column is a typed error, never mis-encoded.
	ErrJSONNotSurfaced
)

// ValueError is a typed value-codec failure. Its message is client-safe: it
// names the malformed shape, never an internal type.
type ValueError struct {
	Code ValueErrorCode
	// Tag carries the offending byte for ErrUnknownTag / ErrInvalidBool.
	Tag byte
}

func (e *ValueError) Error() string {
	switch e.Code {
	case ErrUnsupported:
		return "a value of an unsupported kind cannot cross the wire"
	case ErrTooLarge:
		return "an encoded value exceeds the maximum length"
	case ErrTruncated:
		return "an encoded value ended mid-field"
	case ErrUnknownTag:
		return fmt.Sprintf("unknown encoded value tag %d", e.Tag)
	case ErrInvalidBool:
		return fmt.Sprintf("invalid boolean byte %d", e.Tag)
	case ErrNonUTF8:
		return "an encoded string is not valid utf-8"
	case ErrDepthExceeded:
		return "an encoded value nests too deeply"
	case ErrTrailingBytes:
		return "an encoded value has trailing bytes"
	case ErrRowNotArray:
		return fmt.Sprintf("a query row payload is not an array of columns (tag %d)", e.Tag)
	case ErrJSONNotSurfaced:
		return "a json column cannot be surfaced as json yet"
	default:
		return "invalid encoded value"
	}
}
