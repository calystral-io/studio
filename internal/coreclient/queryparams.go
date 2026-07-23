package coreclient

import (
	"fmt"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/cybrwire"
)

// encodeQueryParams encodes a read's positional parameter VALUES into the cybr
// wire form querypb.QueryRequest.Params carries.
//
// cyqlc lowers each read predicate's compared value (the WHERE literal the CyQL
// builders emit) to a cybr LOAD_PARAM slot, in first-appearance order; the
// builder collects the values in that same order, and Core binds params[i] into
// slot i. Without this the filter value never reaches Core, so a WHERE-filtered
// read cannot execute.
//
// Returns nil for an unfiltered read (no params). A value that cannot encode is
// an internal error - the builders only produce encodable scalars (strings,
// ints), so it should not happen; it is never the API caller's fault.
func encodeQueryParams(vals []cybrwire.Value) ([][]byte, error) {
	if len(vals) == 0 {
		return nil, nil
	}
	out := make([][]byte, len(vals))
	for i, v := range vals {
		b, err := cybrwire.EncodeValue(v)
		if err != nil {
			return nil, apierr.Internal(fmt.Sprintf("encode query param %d: %v", i, err))
		}
		out[i] = b
	}
	return out, nil
}
