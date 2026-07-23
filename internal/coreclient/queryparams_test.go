package coreclient

import (
	"testing"

	"github.com/calystral-io/studio/internal/cybrwire"
)

// cybrStrings decodes each collected param value back to its string, failing on
// a non-string, so a test can assert the builder collected the right values.
func cybrStrings(t *testing.T, vals []cybrwire.Value) []string {
	t.Helper()
	out := make([]string, len(vals))
	for i, v := range vals {
		s, ok := v.AsString()
		if !ok {
			t.Fatalf("param %d is not a string (kind %d)", i, v.Kind())
		}
		out[i] = s
	}
	return out
}

// TestEncodeQueryParams: each filter value encodes to the shared cybr wire form
// (byte-identical to cybrwire.EncodeValue) and round-trips back to its string;
// an empty param list yields nil, i.e. an unfiltered read sends no params.
func TestEncodeQueryParams(t *testing.T) {
	got, err := encodeQueryParams(nil)
	if err != nil || got != nil {
		t.Fatalf("empty = (%v, %v), want (nil, nil)", got, err)
	}

	vals := []cybrwire.Value{cybrwire.Str("us-east"), cybrwire.Str("draining")}
	enc, err := encodeQueryParams(vals)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(enc) != 2 {
		t.Fatalf("len = %d, want 2", len(enc))
	}
	for i, v := range vals {
		want, _ := cybrwire.EncodeValue(v)
		if string(enc[i]) != string(want) {
			t.Errorf("param %d bytes = % x, want % x", i, enc[i], want)
		}
		dec, err := cybrwire.DecodeValue(enc[i])
		if err != nil {
			t.Fatalf("decode param %d: %v", i, err)
		}
		s, _ := dec.AsString()
		wantS, _ := v.AsString()
		if s != wantS {
			t.Errorf("param %d round-trip = %q, want %q", i, s, wantS)
		}
	}
}
