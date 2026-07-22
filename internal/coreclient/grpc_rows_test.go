package coreclient

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/auth"
	"github.com/calystral-io/studio/internal/corepb/querypb"
	"github.com/calystral-io/studio/internal/cybrwire"
)

// rowsQueryServer returns a fixed set of rows for every Query, mirroring a live
// Core whose read path executes. Each configured payload is opaque cybr bytes.
type rowsQueryServer struct {
	querypb.UnimplementedQueryServiceServer
	rows []*querypb.QueryRow
}

func (s *rowsQueryServer) Query(context.Context, *querypb.QueryRequest) (*querypb.QueryResponse, error) {
	return &querypb.QueryResponse{Rows: s.rows}, nil
}

func startStubCoreRows(t *testing.T, rows []*querypb.QueryRow) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	querypb.RegisterQueryServiceServer(srv, &rowsQueryServer{rows: rows})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// nodeIDRow encodes a single-column row [Int(id)] exactly as Core's encode_row
// emits it for a bare-variable projection (anchor normalized to Int(node_id)).
func nodeIDRow(t *testing.T, id int64) *querypb.QueryRow {
	t.Helper()
	payload, err := cybrwire.EncodeValue(cybrwire.Array([]cybrwire.Value{cybrwire.Int(id)}))
	if err != nil {
		t.Fatalf("encode row: %v", err)
	}
	return &querypb.QueryRow{Payload: payload}
}

func reader() *auth.Principal {
	return &auth.Principal{TenantID: "demo-tenant", Roles: []string{"reader"}}
}

func TestListAnchorsSurfacesRows(t *testing.T) {
	addr := startStubCoreRows(t, []*querypb.QueryRow{nodeIDRow(t, 5), nodeIDRow(t, 42)})
	c := newTestGRPCClient(t, addr)

	res, err := c.ListAnchors(context.Background(), ListAnchorsParams{
		TenantID: "demo-tenant", PageSize: 25, Principal: reader(),
	})
	if err != nil {
		t.Fatalf("ListAnchors: %v", err)
	}
	if res.Source != SourceCore {
		t.Errorf("Source = %q, want %q", res.Source, SourceCore)
	}
	if len(res.Items) != 2 || res.Items[0].ID != "5" || res.Items[1].ID != "42" {
		t.Fatalf("items = %+v, want ids [5 42]", res.Items)
	}
	if res.Page.PageSize != 25 || res.Page.TotalEstimate != 2 {
		t.Errorf("page = %+v, want size 25 total 2", res.Page)
	}
}

func TestListNodesSurfacesRows(t *testing.T) {
	addr := startStubCoreRows(t, []*querypb.QueryRow{nodeIDRow(t, 7)})
	c := newTestGRPCClient(t, addr)

	res, err := c.ListNodes(context.Background(), ListNodesParams{
		TenantID: "demo-tenant", PageSize: 25, Principal: reader(),
	})
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].ID != "7" || res.Source != SourceCore {
		t.Fatalf("res = %+v, want one node id 7 from core", res)
	}
}

func TestListLedgerEntriesSurfacesRows(t *testing.T) {
	addr := startStubCoreRows(t, []*querypb.QueryRow{nodeIDRow(t, 3)})
	c := newTestGRPCClient(t, addr)

	res, err := c.ListLedgerEntries(context.Background(), ListLedgerEntriesParams{
		TenantID: "demo-tenant", Name: "audit", PageSize: 25, Principal: reader(),
	})
	if err != nil {
		t.Fatalf("ListLedgerEntries: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].ID != "3" || res.Items[0].Ledger != "audit" {
		t.Fatalf("items = %+v, want entry id 3 in ledger audit", res.Items)
	}
}

// An executed query with no matches is an empty item list and a 200 - never a
// 501. This is the case that lights up the console the moment cyqlc coverage
// lands but no data is seeded yet.
func TestListAnchorsEmptyResultIsNotAnError(t *testing.T) {
	addr := startStubCoreRows(t, nil)
	c := newTestGRPCClient(t, addr)

	res, err := c.ListAnchors(context.Background(), ListAnchorsParams{
		TenantID: "demo-tenant", PageSize: 25, Principal: reader(),
	})
	if err != nil {
		t.Fatalf("empty result must not error: %v", err)
	}
	if len(res.Items) != 0 || res.Source != SourceCore {
		t.Fatalf("res = %+v, want empty items from core", res)
	}
}

// system_as_of has no querypb field, so it cannot be honored over gRPC. Even
// though Core returns rows, a request carrying system_as_of must 501 rather than
// silently serve the LATEST view mislabeled as that historical projection.
func TestListAnchorsSystemAsOfRefusedNotSilentlyLatest(t *testing.T) {
	addr := startStubCoreRows(t, []*querypb.QueryRow{nodeIDRow(t, 5)})
	c := newTestGRPCClient(t, addr)

	sysAsOf := time.Unix(1700000000, 0)
	_, err := c.ListAnchors(context.Background(), ListAnchorsParams{
		TenantID: "demo-tenant", PageSize: 25, Principal: reader(), SystemAsOf: &sysAsOf,
	})
	ae, ok := err.(*apierr.APIError)
	if !ok || ae.Code != apierr.CodeUnimplemented {
		t.Fatalf("system_as_of must 501 (never silently serve latest), got %v", err)
	}
	if ae.Params["surface"] != anchorsSurface {
		t.Errorf("surface = %v, want %q", ae.Params["surface"], anchorsSurface)
	}
}

// A row Core encoded that violates the wire contract (here: a bare Int payload
// that is not the top-level ARRAY of columns) is an INTERNAL error, not a 4xx -
// it is Core's fault, not the caller's.
func TestListAnchorsMalformedRowIsInternal(t *testing.T) {
	bad, err := cybrwire.EncodeValue(cybrwire.Int(9)) // not an array -> not a row
	if err != nil {
		t.Fatal(err)
	}
	addr := startStubCoreRows(t, []*querypb.QueryRow{{Payload: bad}})
	c := newTestGRPCClient(t, addr)

	_, err = c.ListAnchors(context.Background(), ListAnchorsParams{
		TenantID: "demo-tenant", PageSize: 25, Principal: reader(),
	})
	ae, ok := err.(*apierr.APIError)
	if !ok || ae.Code != apierr.CodeInternal {
		t.Fatalf("err = %v, want internal error", err)
	}
}
