package cybrwire

import (
	"encoding/base64"
	"errors"
	"reflect"
	"testing"
)

// goldenRow is the exact wire encoding Core pins in core/src/grpc/query.rs
// `encode_row_golden_bytes`: a row of [anchor(5), int(42)] encodes, under the
// shared value codec, as an ARRAY of two INTs (the anchor normalized to id 5).
// This byte vector is copied verbatim from Core's golden test; if Core changes
// the row encoding, this test breaks in lockstep with Core's.
var goldenRow = []byte{
	0x0A, 0x02, 0x00, 0x00, 0x00, // ARRAY tag, length 2 (u32 LE)
	0x02, 5, 0, 0, 0, 0, 0, 0, 0, // INT tag, 5 i64 LE (normalized anchor id)
	0x02, 42, 0, 0, 0, 0, 0, 0, 0, // INT tag, 42 i64 LE
}

func TestDecodeRowGolden(t *testing.T) {
	cols, err := DecodeRow(goldenRow)
	if err != nil {
		t.Fatalf("DecodeRow(golden): %v", err)
	}
	if len(cols) != 2 {
		t.Fatalf("column count = %d, want 2", len(cols))
	}
	for i, want := range []int64{5, 42} {
		got, ok := cols[i].AsInt()
		if !ok || got != want {
			t.Errorf("col[%d] = (%d, %v), want (%d, true)", i, got, ok, want)
		}
	}
}

func TestDecodeRowRoundTrip(t *testing.T) {
	// Encoding the same logical row (anchor already normalized to Int(5) — the
	// BFF never sees anchors, only Core's normalized wire) must reproduce Core's
	// golden bytes exactly. This pins the Go encoder against Core's byte layout.
	row := Array([]Value{Int(5), Int(42)})
	got, err := EncodeValue(row)
	if err != nil {
		t.Fatalf("EncodeValue: %v", err)
	}
	if !reflect.DeepEqual(got, goldenRow) {
		t.Errorf("re-encoded row = % x, want % x", got, goldenRow)
	}
}

func TestValueToJSON(t *testing.T) {
	cases := []struct {
		name string
		in   Value
		want any
	}{
		{"bool", Bool(true), true},
		{"int", Int(-7), int64(-7)},
		{"time", Time(1700000000000), int64(1700000000000)},
		{"dur", Dur(90), int64(90)},
		{"date", Date(20194), int32(20194)},
		{"dec", Dec("3.14"), "3.14"},
		{"str", Str("hello"), "hello"},
		{"blob", Blob([]byte{0xDE, 0xAD}), base64.StdEncoding.EncodeToString([]byte{0xDE, 0xAD})},
		{"array", Array([]Value{Int(5), Str("x")}), []any{int64(5), "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValueToJSON(tc.in)
			if err != nil {
				t.Fatalf("ValueToJSON: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ValueToJSON = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestRowColumnsToJSON(t *testing.T) {
	cols, err := DecodeRow(goldenRow)
	if err != nil {
		t.Fatalf("DecodeRow: %v", err)
	}
	out := make([]any, len(cols))
	for i, c := range cols {
		if out[i], err = ValueToJSON(c); err != nil {
			t.Fatalf("ValueToJSON col %d: %v", i, err)
		}
	}
	if !reflect.DeepEqual(out, []any{int64(5), int64(42)}) {
		t.Errorf("row json = %#v, want [5 42]", out)
	}
}

func TestValueToJSONJSONColumnErrors(t *testing.T) {
	_, err := ValueToJSON(JSON([]byte(`whatever`)))
	var ve *ValueError
	if !errors.As(err, &ve) || ve.Code != ErrJSONNotSurfaced {
		t.Fatalf("json column err = %v, want ErrJSONNotSurfaced", err)
	}
}

func TestDecodeRowRejectsNonArray(t *testing.T) {
	// A bare Int payload (tag 0x02 + i64) is a valid value but not a row: a row
	// is always the top-level ARRAY of columns.
	bare, err := EncodeValue(Int(9))
	if err != nil {
		t.Fatal(err)
	}
	_, err = DecodeRow(bare)
	var ve *ValueError
	if !errors.As(err, &ve) || ve.Code != ErrRowNotArray {
		t.Fatalf("DecodeRow(bare int) err = %v, want ErrRowNotArray", err)
	}
	if ve.Tag != byte(KindInt) {
		t.Errorf("ErrRowNotArray.Tag = %d, want %d", ve.Tag, KindInt)
	}
}

func TestDecodeRowMalformed(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		code ValueErrorCode
	}{
		{"empty", []byte{}, ErrTruncated},
		{"array len exceeds body", []byte{0x0A, 0x05, 0, 0, 0, 0x02, 1, 0, 0, 0, 0, 0, 0, 0}, ErrTruncated},
		{"unknown column tag", []byte{0x0A, 0x01, 0, 0, 0, 0xFF}, ErrUnknownTag},
		{"trailing bytes after row", append(append([]byte{}, goldenRow...), 0x00), ErrTrailingBytes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeRow(tc.in)
			var ve *ValueError
			if !errors.As(err, &ve) || ve.Code != tc.code {
				t.Fatalf("DecodeRow(%s) err = %v, want code %d", tc.name, err, tc.code)
			}
		})
	}
}
