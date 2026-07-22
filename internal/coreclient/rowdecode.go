package coreclient

import (
	"encoding/json"
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
// anchor kind - see core/src/grpc/query.rs encode_row). So every read surface
// that projects a bare node currently receives exactly one integer column, the
// node id; each surface maps those ids into its own DTO identity field. Richer,
// typed columns (labels, properties, bitemporal coords) populate here once Core
// projects them - blocked on the same cyqlc parser coverage that gates
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

// decodeLedgerNames extracts the ledger name from each row of the catalog
// projection `MATCH (l:Ledger) RETURN l.name` - one string column per row. A row
// whose first column is not a string is a contract violation (internal error).
func decodeLedgerNames(rows []*querypb.QueryRow) ([]string, error) {
	names := make([]string, 0, len(rows))
	for i, r := range rows {
		cols, err := cybrwire.DecodeRow(r.GetPayload())
		if err != nil {
			return nil, apierr.Internal(fmt.Sprintf("decode ledger row %d: %v", i, err))
		}
		if len(cols) == 0 {
			return nil, apierr.Internal(fmt.Sprintf("ledger row %d has no columns", i))
		}
		// Require KindStr specifically: AsString also accepts KindDec, but a ledger
		// name is a string, so a decimal (or anything else) in column 0 is a
		// contract violation to catch, not coerce (symmetric with decodeNodeIDRows
		// requiring KindInt).
		if cols[0].Kind() != cybrwire.KindStr {
			return nil, apierr.Internal(
				fmt.Sprintf("ledger row %d column 0 is not a string name (kind %d)", i, cols[0].Kind()))
		}
		name, _ := cols[0].AsString()
		names = append(names, name)
	}
	return names, nil
}

// decodeClusterSummary maps Core's cluster-summary projection into the rollup
// DTO. The query `MATCH (c:Cluster) RETURN c.summary` projects one column - the
// cluster node's `summary` field, which carries the rollup as JSON text - so we
// parse the first row's summary column into a ClusterSummary. Zero rows means no
// :Cluster node is present, an honest empty rollup (found=false), not an error.
//
// NOTE (pre-schema): Core cannot yet discriminate node types - every single-type
// query resolves to type_id 0 - so this projection returns every type_id-0 node.
// We take the first row; a store that holds only the cluster node under that id
// yields a clean rollup. Type-accurate selection lands with Core's schema
// snapshot; a mistyped/garbled summary column is an internal error, not a 4xx.
func decodeClusterSummary(rows []*querypb.QueryRow) (ClusterSummary, bool, error) {
	if len(rows) == 0 {
		return ClusterSummary{}, false, nil
	}
	cols, err := cybrwire.DecodeRow(rows[0].GetPayload())
	if err != nil {
		return ClusterSummary{}, false, apierr.Internal(fmt.Sprintf("decode cluster summary row: %v", err))
	}
	if len(cols) == 0 {
		return ClusterSummary{}, false, apierr.Internal("cluster summary row has no columns")
	}
	text, ok := cols[0].AsString()
	if !ok {
		return ClusterSummary{}, false, apierr.Internal(
			fmt.Sprintf("cluster summary column is not a string (kind %d)", cols[0].Kind()))
	}
	var cs ClusterSummary
	if err := json.Unmarshal([]byte(text), &cs); err != nil {
		return ClusterSummary{}, false, apierr.Internal("cluster summary payload is not valid json")
	}
	return cs, true, nil
}

// gRPCPage builds the pagination envelope for a Core-sourced page. Core paginates
// server-side via the query's ORDER BY / LIMIT, and querypb.QueryRequest carries
// no cursor field yet, so a page is self-contained: there is no next cursor to
// hand back and TotalEstimate is just what this page returned. When Core grows a
// cursor/total on the query wire, thread them here.
func gRPCPage(pageSize, returned int) Page {
	return Page{PageSize: pageSize, TotalEstimate: returned}
}
