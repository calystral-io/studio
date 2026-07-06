package cybrwire

import (
	"bytes"
	"errors"
	"testing"
)

// sampleValues is one representative value of every encodable kind, including a
// nested array, mirroring core/src/proc/wire.rs sample_values so the round-trip
// covers the whole wire surface.
func sampleValues() []Value {
	return []Value{
		Bool(true),
		Bool(false),
		Int(0),
		Int(-1),
		Int(-9223372036854775808), // i64::MIN
		Int(9223372036854775807),  // i64::MAX
		Dec("3.14159"),
		Str("héllo"),
		Str(""),
		Blob([]byte{0x00, 0x01, 0xff}),
		Time(1_700_000_000_000_000_000),
		Date(-19_000),
		Dur(-42),
		JSON([]byte{0x01, 0x02, 0x03}), // opaque canonical-json bytes, passed through
		Array([]Value{Int(1), Str("two")}),
		Array([]Value{Array([]Value{Bool(true)})}), // nested array
	}
}

// TestEveryKindRoundTripsByBytes: decoding then re-encoding reproduces the exact
// bytes for every encodable kind (the total property, holds even for opaque JSON).
func TestEveryKindRoundTripsByBytes(t *testing.T) {
	for _, v := range sampleValues() {
		enc, err := EncodeValue(v)
		if err != nil {
			t.Fatalf("EncodeValue(%v): %v", v.Kind(), err)
		}
		back, err := DecodeValue(enc)
		if err != nil {
			t.Fatalf("DecodeValue(%v): %v", v.Kind(), err)
		}
		reEnc, err := EncodeValue(back)
		if err != nil {
			t.Fatalf("re-EncodeValue(%v): %v", v.Kind(), err)
		}
		if !bytes.Equal(reEnc, enc) {
			t.Errorf("re-encoding decoded %v changed its bytes:\n got %x\nwant %x", v.Kind(), reEnc, enc)
		}
	}
}

// TestEncodingIsStable: the same value encodes to identical bytes every time
// (the property Calvin relies on for byte-identical replay).
func TestEncodingIsStable(t *testing.T) {
	for _, v := range sampleValues() {
		a, err := EncodeValue(v)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		b, err := EncodeValue(v)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("encoding not stable for %v", v.Kind())
		}
	}
}

// TestGoldenBytes pins the exact tag + little-endian layout so this port cannot
// drift from Core's contract (core/src/proc/wire.rs). These vectors are computed
// by hand from the documented layout.
func TestGoldenBytes(t *testing.T) {
	cases := []struct {
		name string
		v    Value
		want []byte
	}{
		{"bool_true", Bool(true), []byte{0x01, 0x01}},
		{"bool_false", Bool(false), []byte{0x01, 0x00}},
		{"int_1", Int(1), []byte{0x02, 1, 0, 0, 0, 0, 0, 0, 0}},
		{"int_neg1", Int(-1), []byte{0x02, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}},
		{"str_hi", Str("hi"), []byte{0x04, 2, 0, 0, 0, 'h', 'i'}},
		{"str_empty", Str(""), []byte{0x04, 0, 0, 0, 0}},
		{"blob", Blob([]byte{0, 1, 255}), []byte{0x05, 3, 0, 0, 0, 0, 1, 255}},
		{"time_0", Time(0), []byte{0x06, 0, 0, 0, 0, 0, 0, 0, 0}},
		{"dur_neg42", Dur(-42), []byte{0x08, 0xd6, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}},
		{"array", Array([]Value{Int(1), Str("two")}), []byte{
			0x0a, 2, 0, 0, 0, // array tag + count 2
			0x02, 1, 0, 0, 0, 0, 0, 0, 0, // Int(1)
			0x04, 3, 0, 0, 0, 't', 'w', 'o', // Str("two")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EncodeValue(tc.v)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Errorf("bytes:\n got %x\nwant %x", got, tc.want)
			}
			// And the golden bytes decode back to the same value's bytes.
			back, err := DecodeValue(tc.want)
			if err != nil {
				t.Fatalf("decode golden: %v", err)
			}
			re, _ := EncodeValue(back)
			if !bytes.Equal(re, tc.want) {
				t.Errorf("golden did not round-trip:\n got %x\nwant %x", re, tc.want)
			}
		})
	}
}

func TestDecodeErrors(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		code ValueErrorCode
	}{
		{"unknown_tag", []byte{0xEE, 0x00}, ErrUnknownTag},
		{"empty_is_truncated", []byte{}, ErrTruncated},
		{"truncated_int", []byte{byte(KindInt), 1, 2, 3}, ErrTruncated},
		{"invalid_bool", []byte{byte(KindBool), 2}, ErrInvalidBool},
		{"non_utf8_str", []byte{byte(KindStr), 1, 0, 0, 0, 0xff}, ErrNonUTF8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeValue(tc.in)
			var ve *ValueError
			if !errors.As(err, &ve) || ve.Code != tc.code {
				t.Fatalf("err = %v, want code %d", err, tc.code)
			}
		})
	}
}

func TestTrailingBytesRejected(t *testing.T) {
	enc, _ := EncodeValue(Bool(true))
	enc = append(enc, 0x00)
	_, err := DecodeValue(enc)
	var ve *ValueError
	if !errors.As(err, &ve) || ve.Code != ErrTrailingBytes {
		t.Fatalf("err = %v, want ErrTrailingBytes", err)
	}
}

// TestHugeArrayCountFailsTruncated: a huge untrusted count must fail by running
// out of bytes, not by pre-allocating.
func TestHugeArrayCountFailsTruncated(t *testing.T) {
	in := []byte{byte(KindArray), 0xff, 0xff, 0xff, 0xff} // count u32::MAX, no elements
	_, err := DecodeValue(in)
	var ve *ValueError
	if !errors.As(err, &ve) || ve.Code != ErrTruncated {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
}

// TestOverDeepNestingRejectedBothWays: nesting one past MaxDepth is refused by
// both encode and decode with DepthExceeded rather than recursing freely.
func TestOverDeepNestingRejectedBothWays(t *testing.T) {
	v := Int(1)
	for i := 0; i <= MaxDepth; i++ {
		v = Array([]Value{v})
	}
	if _, err := EncodeValue(v); err == nil {
		t.Fatal("encode: want DepthExceeded, got nil")
	} else {
		var ve *ValueError
		if !errors.As(err, &ve) || ve.Code != ErrDepthExceeded {
			t.Fatalf("encode err = %v, want ErrDepthExceeded", err)
		}
	}

	// Hand-roll the same over-deep shape on the wire and confirm decode caps it.
	var wire []byte
	for i := 0; i <= MaxDepth; i++ {
		wire = append(wire, byte(KindArray), 1, 0, 0, 0)
	}
	wire = append(wire, byte(KindInt), 1, 0, 0, 0, 0, 0, 0, 0)
	if _, err := DecodeValue(wire); err == nil {
		t.Fatal("decode: want DepthExceeded, got nil")
	} else {
		var ve *ValueError
		if !errors.As(err, &ve) || ve.Code != ErrDepthExceeded {
			t.Fatalf("decode err = %v, want ErrDepthExceeded", err)
		}
	}
}

// TestAccessors confirms decoded values read back through the typed accessors.
func TestAccessors(t *testing.T) {
	if b, ok := mustDecode(t, Bool(true)).AsBool(); !ok || !b {
		t.Error("AsBool")
	}
	if i, ok := mustDecode(t, Int(42)).AsInt(); !ok || i != 42 {
		t.Error("AsInt")
	}
	if s, ok := mustDecode(t, Str("x")).AsString(); !ok || s != "x" {
		t.Error("AsString")
	}
	if raw, ok := mustDecode(t, Blob([]byte{9})).AsBytes(); !ok || !bytes.Equal(raw, []byte{9}) {
		t.Error("AsBytes")
	}
	if d, ok := mustDecode(t, Date(7)).AsDate(); !ok || d != 7 {
		t.Error("AsDate")
	}
	if a, ok := mustDecode(t, Array([]Value{Int(1)})).AsArray(); !ok || len(a) != 1 {
		t.Error("AsArray")
	}
}

func mustDecode(t *testing.T, v Value) Value {
	t.Helper()
	enc, err := EncodeValue(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := DecodeValue(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return back
}
