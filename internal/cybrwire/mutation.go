package cybrwire

import (
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/calystral-io/studio/internal/corepb/mutatepb"
)

// Mutation is one decoded write operation, at Core's wire level: numeric type /
// field ids and raw u64 anchor refs, matching core/src/mutate.rs. Resolving a
// tenant's string type/field/anchor names to these ids is a higher layer's job
// (and is not yet wired); this codec is purely the byte format.
//
// The payload layout selected by Kind (all integers little-endian; a value is a
// u32 length prefix followed by that many EncodeValue bytes):
//
//	CREATE_NODE | u32 type_id, u32 n, n x (u32 field_id, value)
//	CREATE_EDGE | u64 src, u32 edge_type, u64 dst, u32 n, n x (u32 field_id, value)
//	UPDATE      | u64 anchor, u32 field_id, value
//	CLOSE       | u64 anchor
//
// Field maps encode in ascending field-id order so an unchanged mutation encodes
// to identical bytes (Calvin byte-stability).
type Mutation struct {
	kind mutatepb.MutationKind

	typeID   uint32           // CreateNode
	src      uint64           // CreateEdge
	edgeType uint32           // CreateEdge
	dst      uint64           // CreateEdge
	anchor   uint64           // Update, Close
	field    uint32           // Update
	value    Value            // Update
	fields   map[uint32]Value // CreateNode fields / CreateEdge props
}

// Kind reports the mutation's proto kind.
func (m Mutation) Kind() mutatepb.MutationKind { return m.kind }

// CreateNode is a create-node mutation of type_id with field_id -> value fields.
func CreateNode(typeID uint32, fields map[uint32]Value) Mutation {
	return Mutation{kind: mutatepb.MutationKind_MUTATION_KIND_CREATE_NODE, typeID: typeID, fields: fields}
}

// CreateEdge is a create-edge mutation of edge_type from src to dst with props.
func CreateEdge(src uint64, edgeType uint32, dst uint64, props map[uint32]Value) Mutation {
	return Mutation{kind: mutatepb.MutationKind_MUTATION_KIND_CREATE_EDGE, src: src, edgeType: edgeType, dst: dst, fields: props}
}

// Update sets one field of an existing anchor to value.
func Update(anchor uint64, field uint32, value Value) Mutation {
	return Mutation{kind: mutatepb.MutationKind_MUTATION_KIND_UPDATE, anchor: anchor, field: field, value: value}
}

// Close logically closes (deletes) an anchor.
func Close(anchor uint64) Mutation {
	return Mutation{kind: mutatepb.MutationKind_MUTATION_KIND_CLOSE, anchor: anchor}
}

// Fields returns the create-node fields / create-edge props (nil for other kinds).
func (m Mutation) Fields() map[uint32]Value { return m.fields }

// TypeID returns the create-node type id.
func (m Mutation) TypeID() uint32 { return m.typeID }

// Edge returns the create-edge (src, edge_type, dst).
func (m Mutation) Edge() (uint64, uint32, uint64) { return m.src, m.edgeType, m.dst }

// UpdateTarget returns the update (anchor, field, value).
func (m Mutation) UpdateTarget() (uint64, uint32, Value) { return m.anchor, m.field, m.value }

// Anchor returns the close/update anchor id.
func (m Mutation) Anchor() uint64 { return m.anchor }

// EncodeMutation encodes a mutation to its payload bytes (the Mutation.payload
// on the wire; the kind travels in the proto field, not the payload).
func EncodeMutation(m Mutation) ([]byte, error) {
	var out []byte
	switch m.kind {
	case mutatepb.MutationKind_MUTATION_KIND_CREATE_NODE:
		out = binary.LittleEndian.AppendUint32(out, m.typeID)
		var err error
		if out, err = writeFields(out, m.fields); err != nil {
			return nil, err
		}
	case mutatepb.MutationKind_MUTATION_KIND_CREATE_EDGE:
		out = binary.LittleEndian.AppendUint64(out, m.src)
		out = binary.LittleEndian.AppendUint32(out, m.edgeType)
		out = binary.LittleEndian.AppendUint64(out, m.dst)
		var err error
		if out, err = writeFields(out, m.fields); err != nil {
			return nil, err
		}
	case mutatepb.MutationKind_MUTATION_KIND_UPDATE:
		out = binary.LittleEndian.AppendUint64(out, m.anchor)
		out = binary.LittleEndian.AppendUint32(out, m.field)
		var err error
		if out, err = writeMutationValue(out, m.value); err != nil {
			return nil, err
		}
	case mutatepb.MutationKind_MUTATION_KIND_CLOSE:
		out = binary.LittleEndian.AppendUint64(out, m.anchor)
	default:
		return nil, &MutationError{Code: MErrUnspecifiedKind}
	}
	return out, nil
}

// DecodeMutation decodes a mutation of kind from its payload bytes, requiring
// every byte to be consumed.
func DecodeMutation(kind mutatepb.MutationKind, payload []byte) (Mutation, error) {
	cur := &cursor{buf: payload}
	var m Mutation
	switch kind {
	case mutatepb.MutationKind_MUTATION_KIND_CREATE_NODE:
		typeID, err := readU32(cur)
		if err != nil {
			return Mutation{}, err
		}
		fields, err := readFields(cur)
		if err != nil {
			return Mutation{}, err
		}
		m = CreateNode(typeID, fields)
	case mutatepb.MutationKind_MUTATION_KIND_CREATE_EDGE:
		src, err := readU64(cur)
		if err != nil {
			return Mutation{}, err
		}
		edgeType, err := readU32(cur)
		if err != nil {
			return Mutation{}, err
		}
		dst, err := readU64(cur)
		if err != nil {
			return Mutation{}, err
		}
		props, err := readFields(cur)
		if err != nil {
			return Mutation{}, err
		}
		m = CreateEdge(src, edgeType, dst, props)
	case mutatepb.MutationKind_MUTATION_KIND_UPDATE:
		anchor, err := readU64(cur)
		if err != nil {
			return Mutation{}, err
		}
		field, err := readU32(cur)
		if err != nil {
			return Mutation{}, err
		}
		value, err := readMutationValue(cur)
		if err != nil {
			return Mutation{}, err
		}
		m = Update(anchor, field, value)
	case mutatepb.MutationKind_MUTATION_KIND_CLOSE:
		anchor, err := readU64(cur)
		if err != nil {
			return Mutation{}, err
		}
		m = Close(anchor)
	default:
		return Mutation{}, &MutationError{Code: MErrUnspecifiedKind}
	}
	if !cur.empty() {
		return Mutation{}, &MutationError{Code: MErrTrailingBytes}
	}
	return m, nil
}

// writeFields writes a u32 field count then each (field_id, value) in ascending
// id order (deterministic, Calvin-stable).
func writeFields(out []byte, fields map[uint32]Value) ([]byte, error) {
	if int64(len(fields)) > int64(^uint32(0)) {
		return nil, &MutationError{Code: MErrTooManyFields}
	}
	out = binary.LittleEndian.AppendUint32(out, uint32(len(fields)))
	ids := make([]uint32, 0, len(fields))
	for id := range fields {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		out = binary.LittleEndian.AppendUint32(out, id)
		var err error
		if out, err = writeMutationValue(out, fields[id]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// readFields reads a u32-counted (field_id, value) map. It does not pre-allocate
// count entries: the count is untrusted, so a truncated body with a huge count
// fails by running out of bytes.
func readFields(cur *cursor) (map[uint32]Value, error) {
	count, err := readU32(cur)
	if err != nil {
		return nil, err
	}
	fields := make(map[uint32]Value)
	for i := uint32(0); i < count; i++ {
		id, err := readU32(cur)
		if err != nil {
			return nil, err
		}
		v, err := readMutationValue(cur)
		if err != nil {
			return nil, err
		}
		fields[id] = v
	}
	return fields, nil
}

// writeMutationValue appends a u32-length-prefixed EncodeValue run.
func writeMutationValue(out []byte, v Value) ([]byte, error) {
	b, err := EncodeValue(v)
	if err != nil {
		var ve *ValueError
		if asValueError(err, &ve) {
			return nil, &MutationError{Code: MErrValue, Value: ve}
		}
		return nil, err
	}
	if int64(len(b)) > int64(^uint32(0)) {
		return nil, &MutationError{Code: MErrTooManyFields}
	}
	out = binary.LittleEndian.AppendUint32(out, uint32(len(b)))
	return append(out, b...), nil
}

// readMutationValue reads a u32-length-prefixed value and decodes it, requiring
// the inner value to consume exactly that many bytes.
func readMutationValue(cur *cursor) (Value, error) {
	n, err := readU32(cur)
	if err != nil {
		return Value{}, err
	}
	raw, err := cur.take(int(n))
	if err != nil {
		return Value{}, &MutationError{Code: MErrTruncated}
	}
	v, err := DecodeValue(raw)
	if err != nil {
		var ve *ValueError
		if asValueError(err, &ve) {
			return Value{}, &MutationError{Code: MErrValue, Value: ve}
		}
		return Value{}, err
	}
	return v, nil
}

// readU32 / readU64 read a fixed-width little-endian integer, mapping the
// cursor's truncation to a mutation-level Truncated error.
func readU32(cur *cursor) (uint32, error) {
	v, err := cur.u32()
	if err != nil {
		return 0, &MutationError{Code: MErrTruncated}
	}
	return v, nil
}

func readU64(cur *cursor) (uint64, error) {
	v, err := cur.u64()
	if err != nil {
		return 0, &MutationError{Code: MErrTruncated}
	}
	return v, nil
}

// MutationErrorCode enumerates the ways a mutation payload fails to encode or
// decode. Mirrors core/src/mutate.rs MutationCodecError.
type MutationErrorCode int

// Mutation codec failure codes.
const (
	// MErrUnspecifiedKind: the MutationKind was unspecified or unknown.
	MErrUnspecifiedKind MutationErrorCode = iota
	// MErrTruncated: the payload ended before a field was fully read.
	MErrTruncated
	// MErrTrailingBytes: bytes remained after the mutation decoded.
	MErrTrailingBytes
	// MErrTooManyFields: a field/value count did not fit its u32 prefix.
	MErrTooManyFields
	// MErrValue: an embedded field value did not encode/decode (see Value).
	MErrValue
)

// MutationError is a typed mutation-codec failure with a client-safe message.
type MutationError struct {
	Code MutationErrorCode
	// Value carries the underlying value-codec error when Code == MErrValue.
	Value *ValueError
}

func (e *MutationError) Error() string {
	switch e.Code {
	case MErrUnspecifiedKind:
		return "mutation kind is unspecified"
	case MErrTruncated:
		return "mutation payload ended mid-field"
	case MErrTrailingBytes:
		return "mutation payload has trailing bytes"
	case MErrTooManyFields:
		return "mutation field count exceeds the payload"
	case MErrValue:
		return fmt.Sprintf("mutation field value: %v", e.Value)
	default:
		return "invalid mutation payload"
	}
}

// Unwrap exposes the embedded value error for errors.Is/As.
func (e *MutationError) Unwrap() error {
	if e.Code == MErrValue && e.Value != nil {
		return e.Value
	}
	return nil
}

// asValueError reports whether err is (or wraps) a *ValueError, binding it.
func asValueError(err error, target **ValueError) bool {
	ve, ok := err.(*ValueError)
	if ok {
		*target = ve
	}
	return ok
}
