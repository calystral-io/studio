package cybrwire

import (
	"bytes"
	"errors"
	"testing"

	"github.com/calystral-io/studio/internal/corepb/mutatepb"
)

func sampleMutations() []Mutation {
	fields := map[uint32]Value{1: Int(42), 7: Str("name"), 3: Bool(true)}
	return []Mutation{
		CreateNode(5, fields),
		CreateNode(1, map[uint32]Value{}), // no fields
		CreateEdge(100, 2, 200, fields),
		Update(42, 9, Blob([]byte{0x00, 0x01, 0xff})),
		Close(77),
	}
}

// TestMutationRoundTripsByBytes: decode then re-encode reproduces the exact
// payload bytes for every kind (mirrors core/src/mutate.rs round_trip).
func TestMutationRoundTripsByBytes(t *testing.T) {
	for _, m := range sampleMutations() {
		enc, err := EncodeMutation(m)
		if err != nil {
			t.Fatalf("EncodeMutation(%v): %v", m.Kind(), err)
		}
		back, err := DecodeMutation(m.Kind(), enc)
		if err != nil {
			t.Fatalf("DecodeMutation(%v): %v", m.Kind(), err)
		}
		if back.Kind() != m.Kind() {
			t.Errorf("kind changed: %v -> %v", m.Kind(), back.Kind())
		}
		reEnc, err := EncodeMutation(back)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		if !bytes.Equal(reEnc, enc) {
			t.Errorf("re-encode changed bytes for %v:\n got %x\nwant %x", m.Kind(), reEnc, enc)
		}
	}
}

// TestMutationGoldenBytes pins the payload layout against core/src/mutate.rs.
func TestMutationGoldenBytes(t *testing.T) {
	cases := []struct {
		name string
		m    Mutation
		want []byte
	}{
		{"close", Close(77), []byte{0x4d, 0, 0, 0, 0, 0, 0, 0}},
		{"create_node", CreateNode(5, map[uint32]Value{1: Int(42)}), []byte{
			5, 0, 0, 0, // type_id
			1, 0, 0, 0, // field count
			1, 0, 0, 0, // field_id 1
			9, 0, 0, 0, // value len 9
			0x02, 42, 0, 0, 0, 0, 0, 0, 0, // Int(42)
		}},
		{"update", Update(42, 9, Str("hi")), []byte{
			42, 0, 0, 0, 0, 0, 0, 0, // anchor u64
			9, 0, 0, 0, // field_id
			7, 0, 0, 0, // value len 7
			0x04, 2, 0, 0, 0, 'h', 'i', // Str("hi")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EncodeMutation(tc.m)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Errorf("bytes:\n got %x\nwant %x", got, tc.want)
			}
		})
	}
}

// TestFieldsEncodeInAscendingOrder: the same fields inserted differently encode
// identically (deterministic, Calvin-stable).
func TestFieldsEncodeInAscendingOrder(t *testing.T) {
	a, err := EncodeMutation(CreateNode(1, map[uint32]Value{9: Int(1), 2: Int(2)}))
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncodeMutation(CreateNode(1, map[uint32]Value{2: Int(2), 9: Int(1)}))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("field order not deterministic:\n%x\n%x", a, b)
	}
}

func TestMutationDecodeErrors(t *testing.T) {
	t.Run("unspecified_kind", func(t *testing.T) {
		_, err := DecodeMutation(mutatepb.MutationKind_MUTATION_KIND_UNSPECIFIED, []byte{})
		assertMutationCode(t, err, MErrUnspecifiedKind)
	})
	t.Run("truncated_close", func(t *testing.T) {
		// CLOSE expects 8 anchor bytes; give 3.
		_, err := DecodeMutation(mutatepb.MutationKind_MUTATION_KIND_CLOSE, []byte{1, 2, 3})
		assertMutationCode(t, err, MErrTruncated)
	})
	t.Run("trailing_bytes", func(t *testing.T) {
		payload, _ := EncodeMutation(Close(1))
		payload = append(payload, 0xff)
		_, err := DecodeMutation(mutatepb.MutationKind_MUTATION_KIND_CLOSE, payload)
		assertMutationCode(t, err, MErrTrailingBytes)
	})
	t.Run("huge_field_count", func(t *testing.T) {
		// CREATE_NODE: type_id + u32::MAX field count + no field bytes.
		payload := []byte{5, 0, 0, 0, 0xff, 0xff, 0xff, 0xff}
		_, err := DecodeMutation(mutatepb.MutationKind_MUTATION_KIND_CREATE_NODE, payload)
		assertMutationCode(t, err, MErrTruncated)
	})
	t.Run("bad_embedded_value", func(t *testing.T) {
		// UPDATE with a value whose bytes carry an unknown value tag.
		payload := []byte{
			1, 0, 0, 0, 0, 0, 0, 0, // anchor
			0, 0, 0, 0, // field
			1, 0, 0, 0, // value len 1
			0xEE, // unknown value tag
		}
		_, err := DecodeMutation(mutatepb.MutationKind_MUTATION_KIND_UPDATE, payload)
		assertMutationCode(t, err, MErrValue)
		// And the wrapped value error is inspectable.
		var ve *ValueError
		if !errors.As(err, &ve) || ve.Code != ErrUnknownTag {
			t.Fatalf("wrapped err = %v, want ErrUnknownTag", err)
		}
	})
}

func assertMutationCode(t *testing.T, err error, code MutationErrorCode) {
	t.Helper()
	var me *MutationError
	if !errors.As(err, &me) || me.Code != code {
		t.Fatalf("err = %v, want mutation code %d", err, code)
	}
}
