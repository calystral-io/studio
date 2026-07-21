package cybrwire

import "encoding/base64"

// DecodeRow decodes one query result row into its projected columns.
//
// Core encodes every result row as a single cybr value that is ALWAYS a
// KindArray of the projected columns — see Core core/src/grpc/query.rs
// `encode_row`, which calls `encode_value(&Value::Array(cols))`. A bare-variable
// projection (`RETURN n`) yields one column: the matched node's anchor,
// normalized by Core to Int(node_id) because the shared value codec has no
// anchor kind. DecodeRow returns the columns in projection order.
//
// A payload whose top-level value is not an array is a contract violation
// (ErrRowNotArray): a well-formed row is always an array, even a single-column
// one. Truncation, unknown tags, and trailing bytes surface as the same typed
// ValueError DecodeValue already returns.
func DecodeRow(payload []byte) ([]Value, error) {
	v, err := DecodeValue(payload)
	if err != nil {
		return nil, err
	}
	cols, ok := v.AsArray()
	if !ok {
		return nil, &ValueError{Code: ErrRowNotArray, Tag: byte(v.Kind())}
	}
	return cols, nil
}

// ValueToJSON converts a decoded cybr Value into a JSON-native Go value, so a
// row column marshals straight into an API response. The mapping mirrors Core's
// value kinds:
//
//	Bool  -> bool
//	Int   -> int64            (JSON number)
//	Time  -> int64            (cvm i64 instant; the caller interprets the epoch)
//	Dur   -> int64            (cvm i64 duration)
//	Date  -> int32            (cvm i32 day)
//	Dec   -> string           (canonical decimal text — NOT a float, to keep precision)
//	Str   -> string
//	Blob  -> string           (standard base64; JSON has no byte type)
//	Array -> []any            (recursive, MaxDepth-bounded by DecodeRow/DecodeValue)
//
// A Json column is a typed error (ErrJSONNotSurfaced): its bytes are cvm
// canonical BINARY json, not text, so emitting them as-is would be invalid JSON.
// An unknown kind is likewise a typed error, never a panic.
func ValueToJSON(v Value) (any, error) {
	switch v.Kind() {
	case KindBool:
		b, _ := v.AsBool()
		return b, nil
	case KindInt, KindTime, KindDur:
		i, _ := v.AsInt()
		return i, nil
	case KindDate:
		d, _ := v.AsDate()
		return d, nil
	case KindDec, KindStr:
		s, _ := v.AsString()
		return s, nil
	case KindBlob:
		raw, _ := v.AsBytes()
		return base64.StdEncoding.EncodeToString(raw), nil
	case KindJSON:
		return nil, &ValueError{Code: ErrJSONNotSurfaced}
	case KindArray:
		elems, _ := v.AsArray()
		out := make([]any, 0, len(elems))
		for _, e := range elems {
			j, err := ValueToJSON(e)
			if err != nil {
				return nil, err
			}
			out = append(out, j)
		}
		return out, nil
	default:
		return nil, &ValueError{Code: ErrUnknownTag, Tag: byte(v.Kind())}
	}
}
