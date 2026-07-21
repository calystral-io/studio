package coreclient

import (
	"fmt"
	"strconv"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/corepb/querypb"
	"github.com/calystral-io/studio/internal/cybrwire"
)

// decodeNodeIDRows decodes each query row and extracts its node id.
//
// Core's documented v1 read wire is a single column per row: a bare-variable
// projection (`RETURN n`) pushes the matched node's anchor, which Core
// normalizes to Int(node_id) before encoding (the shared value codec has no
// anchor kind — see core/src/grpc/query.rs encode_row). So every read surface
// that projects a bare node currently receives exactly one integer column, the
// node id; each surface maps those ids into its own DTO identity field. Richer,
// typed columns (labels, properties, bitemporal coords) populate here once Core
// projects them — blocked on the same cyqlc parser coverage that gates
// property/ORDER BY/LIMIT queries, not on this decoder.
//
// A row Core encoded that we cannot decode, or whose first column is not an
// integer, is an INTERNAL error: Core emitted bytes that violate its own wire
// contract, which is not the API caller's fault (never surfaced as a 4xx).
func decodeNodeIDRows(rows []*querypb.QueryRow, surface string) ([]string, error) {
	ids := make([]string, 0, len(rows))
	for i, r := range rows {
		cols, err := cybrwire.DecodeRow(r.GetPayload())
		if err != nil {
			return nil, apierr.Internal(fmt.Sprintf("decode %s row %d: %v", surface, i, err))
		}
		if len(cols) == 0 {
			return nil, apierr.Internal(fmt.Sprintf("%s row %d has no columns", surface, i))
		}
		// Require KindInt specifically: AsInt also accepts Time/Dur, but a node id
		// is an Int, so a Time/Dur in column 0 is a contract violation to catch,
		// not silently coerce.
		if cols[0].Kind() != cybrwire.KindInt {
			return nil, apierr.Internal(fmt.Sprintf(
				"%s row %d column 0 is not an integer node id (kind %d)", surface, i, cols[0].Kind()))
		}
		id, _ := cols[0].AsInt()
		ids = append(ids, strconv.FormatInt(id, 10))
	}
	return ids, nil
}

// gRPCPage builds the pagination envelope for a Core-sourced page. Core paginates
// server-side via the query's ORDER BY / LIMIT, and querypb.QueryRequest carries
// no cursor field yet, so a page is self-contained: there is no next cursor to
// hand back and TotalEstimate is just what this page returned. When Core grows a
// cursor/total on the query wire, thread them here.
func gRPCPage(pageSize, returned int) Page {
	return Page{PageSize: pageSize, TotalEstimate: returned}
}
